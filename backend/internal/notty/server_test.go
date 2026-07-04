package notty

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	crdt "notty/internal/ycrdt"
	"notty/internal/yproto"
)

func TestCloneThreadPreservesEmptyArrays(t *testing.T) {
	thread := cloneThread(&Thread{
		ID:                 "thread_1",
		ParticipantIDs:     []string{},
		ParticipantHandles: []string{},
		Messages:           []*ThreadMessage{},
	})
	payload, err := json.Marshal(thread)
	if err != nil {
		t.Fatalf("marshal thread: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode thread: %v", err)
	}
	for _, key := range []string{"participantIds", "participantHandles", "messages"} {
		values, ok := decoded[key].([]any)
		if !ok {
			t.Fatalf("expected %s to be an empty JSON array, got %#v in %s", key, decoded[key], payload)
		}
		if len(values) != 0 {
			t.Fatalf("expected %s to be empty, got %#v", key, values)
		}
	}
}

func TestBackendTextTruncationPreservesUTF8(t *testing.T) {
	input := strings.Repeat("a", 238) + "可见多"
	got := truncateText(input, 240)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateText returned invalid UTF-8: % x", []byte(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("expected truncateText to append ellipsis, got %q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("truncateText split a valid UTF-8 rune: %q", got)
	}

	fixed := truncateString(input, 240)
	if !utf8.ValidString(fixed) {
		t.Fatalf("truncateString returned invalid UTF-8: % x", []byte(fixed))
	}
	if runeCount(fixed) > 240 {
		t.Fatalf("expected fixed-width truncation to stay within limit, got %d runes", runeCount(fixed))
	}
}

func TestBackendTextTruncationSanitizesInvalidUTF8(t *testing.T) {
	input := "prefix " + string([]byte{0xe5, 0x8f}) + " suffix"
	got := truncateText(input, 240)
	if !utf8.ValidString(got) {
		t.Fatalf("truncateText returned invalid UTF-8: % x", []byte(got))
	}
	if !strings.ContainsRune(got, utf8.RuneError) {
		t.Fatalf("expected invalid bytes to be replaced, got %q", got)
	}
}

func TestWorkspaceEndpointsOmitDocumentPayloads(t *testing.T) {
	fixture := newWorkspaceRouteTestFixture(t)
	documentID := mustCreateTestDocument(t, fixture.store, "docs/spec.md", "sync me")

	router := fixture.router
	var payload map[string]any
	authTestJSON(t, router, http.MethodGet, fixture.workspaceAPIPath("/workspace"), fixture.token, nil, http.StatusOK, &payload)
	if _, ok := payload["proposals"]; ok {
		t.Fatal("expected workspace endpoint to omit removed proposals state")
	}
	if _, ok := payload["documents"]; ok {
		t.Fatal("expected workspace endpoint to omit backend document list")
	}

	assertNonOK(t, router, http.MethodGet, "/api/workspace/sync", nil)
	assertNonOK(t, router, http.MethodGet, "/api/documents", nil)
	assertNonOK(t, router, http.MethodGet, "/api/documents/by-path?path=docs/spec.md", nil)
	assertNonOK(t, router, http.MethodGet, "/api/documents/"+documentID, nil)
	assertNonOK(t, router, http.MethodPost, "/api/documents/"+documentID+"/sync", []byte(`{"stateVector":""}`))
	assertNonOK(t, router, http.MethodPost, "/api/proposals", []byte(`{}`))
}

func TestWorkspaceExposesRootDocumentIDAndHidesRootDocument(t *testing.T) {
	fixture := newWorkspaceRouteTestFixture(t)
	server := fixture.server
	store := fixture.store
	rootID := store.Snapshot().RootDocumentID
	if rootID == "" {
		t.Fatal("expected root document id")
	}
	if !store.HasDocument(rootID) {
		t.Fatalf("root document %q is not syncable", rootID)
	}

	var payload map[string]any
	authTestJSON(t, fixture.router, http.MethodGet, fixture.workspaceAPIPath("/workspace"), fixture.token, nil, http.StatusOK, &payload)
	if payload["rootDocumentId"] != rootID {
		t.Fatalf("rootDocumentId = %#v, want %q", payload["rootDocumentId"], rootID)
	}
	if _, ok := payload["documents"]; ok {
		t.Fatalf("workspace REST response must not expose document namespace: %#v", payload["documents"])
	}

	assertNonOK(t, server.Routes(), http.MethodGet, "/api/documents/by-path?path=.notty/root", nil)
}

func TestWorkspaceEventSocketSnapshotOmitsDocumentList(t *testing.T) {
	fixture := newWorkspaceRouteTestFixture(t)
	server := fixture.server
	store := fixture.store
	_ = mustCreateTestDocument(t, store, "docs/spec.md", "hello")
	httpServer := httptest.NewServer(server.Routes())
	defer httpServer.Close()

	conn := dialDocumentWebsocketForTest(t, httpServer.URL, fixture.workspaceWSPath(""), fixture.token)
	defer conn.Close()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}
	var event EventEnvelope
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("read workspace event snapshot: %v", err)
	}
	if event.Type != "workspace.snapshot" {
		t.Fatalf("workspace event type = %q, want workspace.snapshot", event.Type)
	}
	data, ok := event.Data.(map[string]interface{})
	if !ok {
		t.Fatalf("workspace snapshot data = %#v, want object", event.Data)
	}
	if _, ok := data["documents"]; ok {
		t.Fatalf("workspace event socket snapshot must not carry documents: %#v", data["documents"])
	}
	if data["rootDocumentId"] != store.Snapshot().RootDocumentID {
		t.Fatalf("rootDocumentId = %#v, want %q", data["rootDocumentId"], store.Snapshot().RootDocumentID)
	}
}

func TestRootDocumentDoesNotBootstrapLegacyVisibleDocumentsOnReload(t *testing.T) {
	_, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/legacy.md", "legacy")
	if err := store.Reload(); err != nil {
		t.Fatalf("reload store: %v", err)
	}

	rootID := store.Snapshot().RootDocumentID
	rootDoc := syncDocumentToDocForTest(t, store, rootID, 909)
	if rootEntryExistsForDocumentForTest(t, rootDoc, documentID) {
		t.Fatalf("legacy backend document path for %s must not bootstrap a root entry", documentID)
	}
}

func TestCreateDocumentAllocatesEmptyPathlessStream(t *testing.T) {
	_, store := newTestServer(t)
	first, err := store.CreateDocument(CreateDocumentRequest{}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create first document: %v", err)
	}
	second, err := store.CreateDocument(CreateDocumentRequest{}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create second document: %v", err)
	}
	if first.ID == second.ID {
		t.Fatalf("pathless document creates must allocate different streams: %q", first.ID)
	}
	if got := syncedDocumentTextForTest(t, store, first.ID); got != "" {
		t.Fatalf("created stream should start empty, got %q", got)
	}
	if got := syncedDocumentTextForTest(t, store, second.ID); got != "" {
		t.Fatalf("second created stream should start empty, got %q", got)
	}
}

func TestCreateDocumentAcceptsClientDocumentIDIdempotently(t *testing.T) {
	_, store := newTestServer(t)
	documentID := "doc_11111111-1111-4111-8111-111111111111"
	req := CreateDocumentRequest{
		DocumentID:        documentID,
		ClientOperationID: "local-create-1",
	}
	first, err := store.CreateDocument(req, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create client document: %v", err)
	}
	second, err := store.CreateDocument(req, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("replay client document create: %v", err)
	}
	if first.ID != documentID || second.ID != documentID {
		t.Fatalf("document IDs = %q/%q, want %q", first.ID, second.ID, documentID)
	}
	if first.ClientIDSeed != second.ClientIDSeed {
		t.Fatalf("idempotent replay allocated a new stream: %d vs %d", first.ClientIDSeed, second.ClientIDSeed)
	}
	if _, err := store.CreateDocument(CreateDocumentRequest{
		DocumentID:        documentID,
		ClientOperationID: "different-local-create",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"}); err == nil {
		t.Fatal("same document ID with a different operation should be rejected")
	}
	if _, err := store.CreateDocument(CreateDocumentRequest{DocumentID: "doc_not-a-uuid"}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"}); err == nil {
		t.Fatal("invalid client document ID should be rejected")
	}
}

func TestDocumentNamespaceMutationHTTPRoutesRemoved(t *testing.T) {
	fixture := newWorkspaceRouteTestFixture(t)
	server := fixture.server
	store := fixture.store
	documentID := mustCreateTestDocument(t, store, "docs/route.md", "content")
	router := server.Routes()
	for _, target := range []string{"/api/documents/" + documentID, "/api/workspaces/" + store.Snapshot().WorkspaceID + "/documents/" + documentID} {
		recorder := authTestRequest(t, router, http.MethodPatch, target, fixture.token, nil, map[string]string{"path": "docs/next.md"})
		if recorder.Code == http.StatusOK {
			t.Fatalf("expected PATCH %s to be unavailable, got status 200 body=%s", target, recorder.Body.String())
		}
		recorder = authTestRequest(t, router, http.MethodDelete, target, fixture.token, nil, nil)
		if recorder.Code == http.StatusOK {
			t.Fatalf("expected DELETE %s to be unavailable, got status 200 body=%s", target, recorder.Body.String())
		}
	}
}

func TestApplyCRDTUpdateDoesNotPersistIdempotentUpdate(t *testing.T) {
	_, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/idempotent.md", "")
	peer := crdt.New()
	defer peer.Close()
	text := peer.GetText("content")
	update := captureDocUpdate(t, peer, "peer", func(txn *crdt.Transaction) {
		text.Insert(txn, 0, "alpha", nil)
	})

	first, err := store.ApplyCRDTUpdateWithResult(documentID, update, OperationMeta{ActorID: "peer", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("apply first update: %v", err)
	}
	if !first.Applied {
		t.Fatal("expected first update to apply")
	}
	second, err := store.ApplyCRDTUpdateWithResult(documentID, update, OperationMeta{ActorID: "peer", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("apply duplicate update: %v", err)
	}
	if second.Applied {
		t.Fatal("expected duplicate update to be ignored")
	}
	if second.Document.UpdateID != first.Document.UpdateID {
		t.Fatalf("duplicate update advanced update id: got %d want %d", second.Document.UpdateID, first.Document.UpdateID)
	}
}

func TestApplyCRDTUpdatePersistsDeleteOnlyUpdate(t *testing.T) {
	_, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/delete-only.md", "")
	peer := crdt.New()
	defer peer.Close()
	text := peer.GetText("content")
	insertUpdate := captureDocUpdate(t, peer, "peer-insert", func(txn *crdt.Transaction) {
		text.Insert(txn, 0, "HEAD\nalpha\nbe-MID-ta\ngamma\nTAIL\n", nil)
	})
	inserted, err := store.ApplyCRDTUpdateWithResult(documentID, insertUpdate, OperationMeta{ActorID: "peer", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("apply insert update: %v", err)
	}
	if !inserted.Applied {
		t.Fatal("expected insert update to apply")
	}

	deleteUpdate := captureDocUpdate(t, peer, "peer-delete", func(txn *crdt.Transaction) {
		text.Delete(txn, len("HEAD\nalpha\nbe-MID-ta\ngamma\n"), len("TAIL\n"))
		text.Delete(txn, len("HEAD\nalpha\nbe"), len("-MID-"))
		text.Delete(txn, 0, len("HEAD\n"))
	})
	deleted, err := store.ApplyCRDTUpdateWithResult(documentID, deleteUpdate, OperationMeta{ActorID: "peer", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("apply delete-only update: %v", err)
	}
	if !deleted.Applied {
		t.Fatal("expected delete-only update to apply")
	}
	if got := currentDocumentContentForTest(t, store, documentID); got != "alpha\nbeta\ngamma\n" {
		t.Fatalf("delete-only update content = %q", got)
	}

	replayed, err := store.ApplyCRDTUpdateWithResult(documentID, deleteUpdate, OperationMeta{ActorID: "peer", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("replay delete-only update: %v", err)
	}
	if replayed.Applied {
		t.Fatal("expected replayed delete-only update to be ignored")
	}
	if replayed.Document.UpdateID != deleted.Document.UpdateID {
		t.Fatalf("replayed delete-only update advanced update id: got %d want %d", replayed.Document.UpdateID, deleted.Document.UpdateID)
	}
}

func TestRootDocumentSyncsOverMuxWebsocket(t *testing.T) {
	fixture := newWorkspaceRouteTestFixture(t)
	server := fixture.server
	store := fixture.store
	rootID := store.Snapshot().RootDocumentID
	httpServer := httptest.NewServer(server.Routes())
	defer httpServer.Close()

	conn := dialDocumentWebsocketForTest(t, httpServer.URL, fixture.workspaceWSPath("/documents-sync"), fixture.token)
	defer conn.Close()
	if err := conn.WriteMessage(websocket.BinaryMessage, yproto.BuildDocumentMessage(rootID, buildSyncStep1ForTest(crdt.New(crdt.WithClientID(707))))); err != nil {
		t.Fatalf("write root sync step1: %v", err)
	}
	frame := readDocumentWebsocketFrameForTest(t, conn)
	if frame.documentID != rootID {
		t.Fatalf("root sync response document = %q, want %q", frame.documentID, rootID)
	}
	messageType, reader, err := yproto.DecodeProtocolMessage(frame.payload)
	if err != nil {
		t.Fatalf("decode root sync response: %v", err)
	}
	if messageType != yproto.MessageSync {
		t.Fatalf("root sync top-level = %d, want sync", messageType)
	}
	syncType, _, err := yproto.DecodeSyncMessage(reader)
	if err != nil {
		t.Fatalf("decode root sync message: %v", err)
	}
	if syncType != yproto.SyncStep2 && syncType != yproto.SyncStep1 {
		t.Fatalf("root sync type = %d, want step2 or step1", syncType)
	}
}

func TestRootDocumentUpdatesStreamToRawRootSubscriber(t *testing.T) {
	fixture := newWorkspaceRouteTestFixture(t)
	server := fixture.server
	store := fixture.store
	rootID := store.Snapshot().RootDocumentID
	httpServer := httptest.NewServer(server.Routes())
	defer httpServer.Close()

	reader := dialDocumentWebsocketForTest(t, httpServer.URL, fixture.workspaceWSPath("/documents/"+rootID), fixture.token)
	defer reader.Close()
	rootRoom := server.rooms.ForDocument(store.Snapshot().WorkspaceID + ":" + rootID)
	waitDocumentRoomSubscriberCount(t, rootRoom, 1)

	writer := dialDocumentWebsocketForTest(t, httpServer.URL, fixture.workspaceWSPath("/documents-sync"), fixture.token)
	defer writer.Close()

	writerDoc := crdt.New(crdt.WithClientID(808))
	defer writerDoc.Close()
	readerDoc := crdt.New(crdt.WithClientID(909))
	defer readerDoc.Close()
	documentID := "doc_streamed_root"

	upsert, err := upsertRootEntryForTest(writerDoc, documentID, "docs/a.md")
	if err != nil {
		t.Fatalf("build root upsert update: %v", err)
	}
	writeMuxRootUpdateForTest(t, writer, rootID, upsert)
	applyRawRootStreamUpdateForTest(t, reader, readerDoc)
	if path, deleted := rootEntryForDocumentForTest(t, readerDoc, documentID); path != "docs/a.md" || deleted {
		t.Fatalf("projected root entry after upsert = path %q deleted %v, want docs/a.md false", path, deleted)
	}

	move, err := upsertRootEntryForTest(writerDoc, documentID, "docs/b.md")
	if err != nil {
		t.Fatalf("build root move update: %v", err)
	}
	writeMuxRootUpdateForTest(t, writer, rootID, move)
	applyRawRootStreamUpdateForTest(t, reader, readerDoc)
	if path, deleted := rootEntryForDocumentForTest(t, readerDoc, documentID); path != "docs/b.md" || deleted {
		t.Fatalf("projected root entry after move = path %q deleted %v, want docs/b.md false", path, deleted)
	}

	tombstone, err := tombstoneRootEntryForTest(writerDoc, documentID)
	if err != nil {
		t.Fatalf("build root tombstone update: %v", err)
	}
	writeMuxRootUpdateForTest(t, writer, rootID, tombstone)
	applyRawRootStreamUpdateForTest(t, reader, readerDoc)
	if _, deleted := rootEntryForDocumentForTest(t, readerDoc, documentID); !deleted {
		t.Fatal("projected root entry should be tombstoned after delete update")
	}
}

func TestAgentDocumentDiffEndpointRejectsLargeDiff(t *testing.T) {
	fixture := newWorkspaceRouteTestFixture(t)
	server := fixture.server
	store := fixture.store
	seedCodexDaemonRuntime(t, store)
	documentID := mustCreateTestDocument(t, store, "docs/large-api-diff.md", numberedLines(2001))
	agent, err := store.CreateAgent(CreateAgentRequest{
		Handle: "reviewer",
		Name:   "Reviewer",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	initial, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get initial document: %v", err)
	}
	if _, _, err := store.ReplaceDocumentText(documentID, numberedLines(2001)+"tail\n", OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("replace document: %v", err)
	}
	current, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get current document: %v", err)
	}

	target := fixture.workspaceAPIPath("/agents/" + agent.ID + "/documents/" + documentID + "/diff?from=" + strconv.FormatInt(initial.UpdateID, 10) + "&to=" + strconv.FormatInt(current.UpdateID, 10))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request.Header.Set("Authorization", "Bearer "+fixture.token)
	server.Routes().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for large diff, got status %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestDocumentProtocolSyncReturnsMissingCRDTUpdate(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "alpha")
	room := server.rooms.ForDocument(documentID)

	clientDoc := crdt.New()
	clientConn := &DocumentConn{send: make(chan []byte, 4)}
	syncClientFromServer(t, server, store, room, clientConn, documentID, clientDoc)
	if got := clientDoc.GetText("content").ToString(); got != "alpha" {
		t.Fatalf("unexpected initial sync content: %q", got)
	}

	serverPeer := syncDocumentToDocForTest(t, store, documentID, 99)
	text := serverPeer.GetText("content")
	update := captureDocUpdate(t, serverPeer, "server-peer", func(txn *crdt.Transaction) {
		text.Insert(txn, text.LenInTxn(txn), " beta", nil)
	})
	if _, err := store.ApplyCRDTUpdate(documentID, update, OperationMeta{ActorID: "peer", ActorType: "agent", Source: "test"}); err != nil {
		t.Fatalf("apply server peer update: %v", err)
	}

	syncClientFromServer(t, server, store, room, clientConn, documentID, clientDoc)
	if got := clientDoc.GetText("content").ToString(); got != "alpha beta" {
		t.Fatalf("delta sync diverged: got %q want %q", got, "alpha beta")
	}
}

func TestDocumentProtocolUpdateDoesNotPublishDocumentNamespaceEvent(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "alpha")
	peerDoc := syncDocumentToDocForTest(t, store, documentID, 77)
	text := peerDoc.GetText("content")
	update := captureDocUpdate(t, peerDoc, "browser", func(txn *crdt.Transaction) {
		text.Insert(txn, text.LenInTxn(txn), " beta", nil)
	})

	events, unsubscribe := server.workspaceBroker(store.Snapshot().WorkspaceID).Subscribe()
	defer unsubscribe()
	source := &DocumentConn{send: make(chan []byte, 4)}
	if err := handleDocumentProtocolMessageForTest(t, server, store, server.rooms.ForDocument(documentID), source, documentID, yproto.BuildSyncUpdate(update), OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("handle document protocol message: %v", err)
	}

	for {
		select {
		case event := <-events:
			if strings.HasPrefix(event.Type, "document.") {
				t.Fatalf("document namespace event must not be published for content update: %#v", event)
			}
		default:
			return
		}
	}
}

func TestHTTPDocumentUpdateRouteRemoved(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/removed-http-update.md", "alpha")

	routes := []struct {
		method string
		target string
		body   []byte
	}{
		{method: http.MethodPost, target: "/api/documents/" + documentID + "/updates?actor=agent_1&actor_type=agent", body: []byte{0, 0}},
		{method: http.MethodPatch, target: "/api/documents/" + documentID, body: []byte(`{"path":"docs/moved.md"}`)},
		{method: http.MethodDelete, target: "/api/documents/" + documentID},
	}
	for _, route := range routes {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(route.method, route.target, bytes.NewReader(route.body))
		request.Header.Set("Content-Type", "application/json")
		server.Routes().ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNotFound && recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected removed HTTP lifecycle route %s %s, got %d body=%s", route.method, route.target, recorder.Code, recorder.Body.String())
		}
	}
}

func TestWorkspaceDocumentSyncWebsocketRoutesMultipleDocuments(t *testing.T) {
	fixture := newWorkspaceRouteTestFixture(t)
	server := fixture.server
	store := fixture.store
	docA := mustCreateTestDocument(t, store, "docs/a.md", "alpha")
	docB := mustCreateTestDocument(t, store, "docs/b.md", "bravo")
	httpServer := httptest.NewServer(server.Routes())
	defer httpServer.Close()

	conn := dialDocumentWebsocketForTest(t, httpServer.URL, fixture.workspaceWSPath("/documents-sync"), fixture.token)
	defer conn.Close()

	if err := conn.WriteMessage(websocket.BinaryMessage, yproto.BuildDocumentMessage(docA, buildSyncStep1ForTest(crdt.New(crdt.WithClientID(101))))); err != nil {
		t.Fatalf("write docA sync step1: %v", err)
	}
	frameA := readDocumentWebsocketFrameForTest(t, conn)
	if frameA.documentID != docA {
		t.Fatalf("first response document = %q, want %q", frameA.documentID, docA)
	}
	assertSyncPayloadForTest(t, frameA.payload)

	if err := conn.WriteMessage(websocket.BinaryMessage, yproto.BuildDocumentMessage(docB, buildSyncStep1ForTest(crdt.New(crdt.WithClientID(202))))); err != nil {
		t.Fatalf("write docB sync step1: %v", err)
	}
	frameB := readDocumentWebsocketFrameForTest(t, conn)
	for frameB.documentID == docA {
		frameB = readDocumentWebsocketFrameForTest(t, conn)
	}
	if frameB.documentID != docB {
		t.Fatalf("second document response = %q, want %q", frameB.documentID, docB)
	}
	assertSyncPayloadForTest(t, frameB.payload)
}

func TestRawDocumentWebsocketStillUsesRawYProtocolFrames(t *testing.T) {
	fixture := newWorkspaceRouteTestFixture(t)
	server := fixture.server
	store := fixture.store
	documentID := mustCreateTestDocument(t, store, "docs/raw.md", "alpha")
	httpServer := httptest.NewServer(server.Routes())
	defer httpServer.Close()

	conn := dialDocumentWebsocketForTest(t, httpServer.URL, fixture.workspaceWSPath("/documents/"+documentID), fixture.token)
	defer conn.Close()
	if err := conn.WriteMessage(websocket.BinaryMessage, buildSyncStep1ForTest(crdt.New(crdt.WithClientID(303)))); err != nil {
		t.Fatalf("write raw sync step1: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read raw websocket response: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("raw websocket response type = %d, want binary", messageType)
	}
	assertSyncPayloadForTest(t, payload)
	if _, _, err := yproto.DecodeDocumentMessage(payload); err == nil {
		t.Fatal("raw websocket response must not be wrapped as a routed document message")
	}
}

func TestWebsocketDocumentUpdateBroadcastsToMuxSubscriber(t *testing.T) {
	fixture := newWorkspaceRouteTestFixture(t)
	server := fixture.server
	store := fixture.store
	documentID := mustCreateTestDocument(t, store, "docs/mux-broadcast.md", "alpha")
	httpServer := httptest.NewServer(server.Routes())
	defer httpServer.Close()

	conn := dialDocumentWebsocketForTest(t, httpServer.URL, fixture.workspaceWSPath("/documents-sync"), fixture.token)
	defer conn.Close()
	subscribePayload := yproto.BuildAwarenessUpdate(map[uint64]yproto.AwarenessState{
		11: {Clock: 1, State: []byte(`{"actorId":"daemon"}`)},
	}, []uint64{11})
	if err := conn.WriteMessage(websocket.BinaryMessage, yproto.BuildDocumentMessage(documentID, subscribePayload)); err != nil {
		t.Fatalf("write mux awareness subscription: %v", err)
	}
	roomKey := store.Snapshot().WorkspaceID + ":" + documentID
	waitDocumentRoomSubscriberCount(t, server.rooms.ForDocument(roomKey), 1)

	peerDoc := syncDocumentToDocForTest(t, store, documentID, 77)
	text := peerDoc.GetText("content")
	update := captureDocUpdate(t, peerDoc, "websocket-edit", func(txn *crdt.Transaction) {
		text.Insert(txn, text.LenInTxn(txn), " beta", nil)
	})
	writer := dialDocumentWebsocketForTest(t, httpServer.URL, fixture.workspaceWSPath("/documents/"+documentID), fixture.token)
	defer writer.Close()
	if err := writer.WriteMessage(websocket.BinaryMessage, yproto.BuildSyncUpdate(update)); err != nil {
		t.Fatalf("write websocket update: %v", err)
	}

	frame := readDocumentWebsocketFrameForTest(t, conn)
	if frame.documentID != documentID {
		t.Fatalf("broadcast document = %q, want %q", frame.documentID, documentID)
	}
	messageType, reader, err := yproto.DecodeProtocolMessage(frame.payload)
	if err != nil {
		t.Fatalf("decode broadcast protocol: %v", err)
	}
	if messageType != yproto.MessageSync {
		t.Fatalf("broadcast top-level = %d, want sync", messageType)
	}
	syncType, data, err := yproto.DecodeSyncMessage(reader)
	if err != nil {
		t.Fatalf("decode broadcast sync: %v", err)
	}
	if syncType != yproto.SyncUpdate {
		t.Fatalf("broadcast sync type = %d, want update", syncType)
	}
	if !bytes.Equal(data, update) {
		t.Fatal("broadcast update bytes did not match websocket update")
	}
}

func TestWorkspaceDocumentSyncWebsocketCloseRemovesAllSubscriptions(t *testing.T) {
	fixture := newWorkspaceRouteTestFixture(t)
	server := fixture.server
	store := fixture.store
	docA := mustCreateTestDocument(t, store, "docs/cleanup-a.md", "alpha")
	docB := mustCreateTestDocument(t, store, "docs/cleanup-b.md", "bravo")
	httpServer := httptest.NewServer(server.Routes())
	defer httpServer.Close()

	conn := dialDocumentWebsocketForTest(t, httpServer.URL, fixture.workspaceWSPath("/documents-sync"), fixture.token)
	awareness := yproto.BuildAwarenessUpdate(map[uint64]yproto.AwarenessState{
		11: {Clock: 1, State: []byte(`{"actorId":"daemon"}`)},
	}, []uint64{11})
	for _, documentID := range []string{docA, docB} {
		if err := conn.WriteMessage(websocket.BinaryMessage, yproto.BuildDocumentMessage(documentID, awareness)); err != nil {
			t.Fatalf("write subscription for %s: %v", documentID, err)
		}
	}
	roomA := server.rooms.ForDocument(store.Snapshot().WorkspaceID + ":" + docA)
	roomB := server.rooms.ForDocument(store.Snapshot().WorkspaceID + ":" + docB)
	waitDocumentRoomSubscriberCount(t, roomA, 1)
	waitDocumentRoomSubscriberCount(t, roomB, 1)

	if err := conn.Close(); err != nil {
		t.Fatalf("close websocket: %v", err)
	}
	waitDocumentRoomSubscriberCount(t, roomA, 0)
	waitDocumentRoomSubscriberCount(t, roomB, 0)
}

func TestBackendMuxTransportDoesNotOwnYProtocolSemantics(t *testing.T) {
	data, err := os.ReadFile("server_documents.go")
	if err != nil {
		t.Fatalf("read server_documents.go: %v", err)
	}
	source := string(data)
	if count := strings.Count(source, "yproto.DecodeSyncMessage"); count != 1 {
		t.Fatalf("y-protocol sync semantics must stay in the canonical handler; DecodeSyncMessage count=%d", count)
	}
	if count := strings.Count(source, "yproto.DecodeDocumentMessage"); count != 1 {
		t.Fatalf("document routing decode should only happen at the frame boundary; count=%d", count)
	}
	if count := strings.Count(source, "yproto.BuildDocumentMessage"); count != 1 {
		t.Fatalf("document routing encode should only happen at the frame boundary; count=%d", count)
	}
}

func TestBackendDoesNotExposeLegacyDocumentNamespaceSurface(t *testing.T) {
	checks := map[string][]string{
		"server_workspace.go": {
			`"documents"`,
			"SortedSyncDocuments",
		},
		"server_documents.go": {
			"DocumentLifecycleEvent",
			"DocumentUpdateEvent",
			`Type: "document.created"`,
			`Type: "document.updated"`,
		},
		"types.go": {
			"DocumentLifecycleEvent",
			"DocumentUpdateEvent",
		},
		"store.go": {
			"SortedSyncDocuments",
			"rootBootstrap",
			"root-document-legacy-bootstrap",
		},
	}
	matches := map[string][]string{}
	for path, forbidden := range checks {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		text := string(data)
		for _, token := range forbidden {
			if strings.Contains(text, token) {
				matches[path] = append(matches[path], token)
			}
		}
		if path == "types.go" {
			source := string(data)
			documentBlock := source[strings.Index(source, "type Document struct {"):strings.Index(source, "type DocumentMetadata struct {")]
			if strings.Contains(documentBlock, "Path") || strings.Contains(documentBlock, "Title") {
				matches[path] = append(matches[path], "Document path/title")
			}
			metadataBlock := source[strings.Index(source, "type DocumentMetadata struct {"):strings.Index(source, "type ThreadAnchor struct {")]
			if strings.Contains(metadataBlock, "Path") || strings.Contains(metadataBlock, "Title") {
				matches[path] = append(matches[path], "DocumentMetadata path/title")
			}
		}
	}
	if len(matches) != 0 {
		t.Fatalf("backend must not expose legacy document namespace surface: %#v", matches)
	}
}

func TestDocumentProtocolIgnoresCanonicalEmptyYjsUpdate(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/empty-update.md", "alpha")
	before, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document before empty update: %v", err)
	}

	events, unsubscribe := server.workspaceBroker(store.Snapshot().WorkspaceID).Subscribe()
	defer unsubscribe()
	source := &DocumentConn{send: make(chan []byte, 4)}
	if err := handleDocumentProtocolMessageForTest(t, server, store, server.rooms.ForDocument(documentID), source, documentID, yproto.BuildSyncUpdate([]byte{0, 0}), OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("handle empty document update: %v", err)
	}

	after, err := store.GetDocument(documentID)
	if err != nil {
		t.Fatalf("get document after empty update: %v", err)
	}
	if after.UpdateID != before.UpdateID {
		t.Fatalf("canonical empty update changed document version: before=%d after=%d", before.UpdateID, after.UpdateID)
	}
	select {
	case event := <-events:
		t.Fatalf("canonical empty update should not publish workspace event, got %#v", event)
	default:
	}
}

func TestWorkspaceEndpointsTrimHistoricalAgentRunPayloads(t *testing.T) {
	fixture := newWorkspaceRouteTestFixture(t)
	store := fixture.store
	seedCodexDaemonRuntime(t, store)
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

	router := fixture.router
	var workspacePayload map[string]any
	authTestJSON(t, router, http.MethodGet, fixture.workspaceAPIPath("/workspace"), fixture.token, nil, http.StatusOK, &workspacePayload)
	workspaceRun := findRunPayload(t, workspacePayload, run.ID)
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

	var terminalPayload map[string]any
	authTestJSON(t, router, http.MethodGet, fixture.workspaceAPIPath("/workspace"), fixture.token, nil, http.StatusOK, &terminalPayload)
	terminalRun := findRunPayload(t, terminalPayload, run.ID)
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
	fixture := newWorkspaceRouteTestFixture(t)
	store := fixture.store
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "alpha bravo charlie")

	router := fixture.router
	body, err := json.Marshal(CreateThreadRequest{
		DocumentID:    documentID,
		Title:         "Question",
		Body:          "Please review this section.",
		RelativeStart: "browser-relative-start",
		RelativeEnd:   "browser-relative-end",
		Excerpt:       "alpha",
	})
	if err != nil {
		t.Fatalf("marshal create thread request: %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, fixture.workspaceAPIPath("/threads?actor=owner&actor_type=human"), bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+fixture.token)
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
	anchorMap, ok := threadMap["anchor"].(map[string]any)
	if !ok {
		t.Fatalf("expected thread anchor in response, got %#v", threadMap["anchor"])
	}
	if anchorMap["relativeStart"] != "browser-relative-start" || anchorMap["relativeEnd"] != "browser-relative-end" {
		t.Fatalf("expected caller-provided relative anchors, got %#v", anchorMap)
	}
	if _, ok := anchorMap["line"]; ok {
		t.Fatalf("line should not be persisted in canonical anchors: %#v", anchorMap)
	}
	if _, ok := anchorMap["start"]; ok {
		t.Fatalf("start should not be persisted in canonical anchors: %#v", anchorMap)
	}
	if _, ok := anchorMap["end"]; ok {
		t.Fatalf("end should not be persisted in canonical anchors: %#v", anchorMap)
	}
	if _, ok := anchorMap["documentId"]; ok {
		t.Fatalf("documentId is already on the thread and should not be duplicated in anchor: %#v", anchorMap)
	}

	var fetched map[string]any
	authTestJSON(t, router, http.MethodGet, fixture.workspaceAPIPath("/threads/"+threadID), fixture.token, nil, http.StatusOK, &fetched)
	gotThread, ok := fetched["thread"].(map[string]any)
	if !ok {
		t.Fatalf("expected fetched thread object, got %#v", fetched["thread"])
	}
	if gotThread["id"] != threadID {
		t.Fatalf("expected fetched thread %q, got %#v", threadID, gotThread["id"])
	}
}

func TestCreateThreadIsIdempotentByClientOperationID(t *testing.T) {
	fixture := newWorkspaceRouteTestFixture(t)
	server := fixture.server
	store := fixture.store
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "alpha bravo charlie")
	router := fixture.router
	events, unsubscribe := server.workspaceBroker(fixture.workspaceID).Subscribe()
	defer unsubscribe()
	body, err := json.Marshal(CreateThreadRequest{
		DocumentID:    documentID,
		Body:          "Please review this section.",
		Kind:          "text-range",
		RelativeStart: "browser-relative-start",
		RelativeEnd:   "browser-relative-end",
	})
	if err != nil {
		t.Fatalf("marshal create thread request: %v", err)
	}
	post := func() map[string]any {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, fixture.workspaceAPIPath("/threads?actor=owner&actor_type=human"), bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Notty-Idempotency-Key", "intent_123")
		request.Header.Set("Authorization", "Bearer "+fixture.token)
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("create thread status = %d, body = %s", recorder.Code, recorder.Body.String())
		}
		var response map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		thread, ok := response["thread"].(map[string]any)
		if !ok {
			t.Fatalf("expected thread object, got %#v", response["thread"])
		}
		return thread
	}

	first := post()
	firstEventTypes := drainBrokerEventTypes(events)
	if len(firstEventTypes) != 2 || firstEventTypes[0] != "thread.created" || firstEventTypes[1] != "thread.message.created" {
		t.Fatalf("expected first create to publish one thread creation and message event, got %#v", firstEventTypes)
	}
	second := post()
	if duplicateEventTypes := drainBrokerEventTypes(events); len(duplicateEventTypes) != 0 {
		t.Fatalf("idempotent replay should not publish duplicate thread events, got %#v", duplicateEventTypes)
	}
	if first["id"] != second["id"] {
		t.Fatalf("expected idempotent repeat to return same thread, got %v and %v", first["id"], second["id"])
	}
	threads, err := store.ListThreadsForDocument(documentID)
	if err != nil {
		t.Fatalf("list threads: %v", err)
	}
	if len(threads) != 1 {
		t.Fatalf("expected one stored thread after duplicate POST, got %d", len(threads))
	}
	if threads[0].ClientOperationID != "intent_123" {
		t.Fatalf("expected client operation ID to persist, got %q", threads[0].ClientOperationID)
	}
}

func drainBrokerEventTypes(events <-chan EventEnvelope) []string {
	var types []string
	for {
		select {
		case event := <-events:
			types = append(types, event.Type)
		default:
			return types
		}
	}
}

func TestHandleDocumentProtocolMessageBroadcastsSyncUpdateToPeers(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "alpha")

	peer := syncDocumentToDocForTest(t, store, documentID, 99)
	var update []byte
	text := peer.GetText("content")
	unsubscribe := peer.OnUpdate(func(next []byte, origin any) {
		if origin == "peer" {
			update = append([]byte(nil), next...)
		}
	})
	peer.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, text.LenInTxn(txn), " bravo", nil)
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

	err := handleDocumentProtocolMessageForTest(t, server, store, room, source, documentID, yproto.BuildSyncUpdate(update), OperationMeta{
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

	if got := currentDocumentContentForTest(t, store, documentID); got != "alpha bravo" {
		t.Fatalf("unexpected updated content: %q", got)
	}
}

func TestHandleDocumentProtocolMessageClosesSlowPeerOnSyncUpdateOverflow(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "alpha")

	peer := syncDocumentToDocForTest(t, store, documentID, 99)
	var update []byte
	text := peer.GetText("content")
	unsubscribe := peer.OnUpdate(func(next []byte, origin any) {
		if origin == "peer" {
			update = append([]byte(nil), next...)
		}
	})
	peer.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, text.LenInTxn(txn), " bravo", nil)
	}, "peer")
	unsubscribe()
	if len(update) == 0 {
		t.Fatal("expected peer update bytes")
	}

	room := server.rooms.ForDocument(documentID)
	source := newDocumentConn(1)
	slowPeer := newDocumentConn(1)
	slowPeer.send <- []byte("queued")
	room.Add(source)
	room.Add(slowPeer)
	defer room.Remove(source)
	defer room.Remove(slowPeer)

	err := handleDocumentProtocolMessageForTest(t, server, store, room, source, documentID, yproto.BuildSyncUpdate(update), OperationMeta{
		ActorID:   "peer",
		ActorType: "human",
		Source:    "test",
	})
	if err != nil {
		t.Fatalf("handle document protocol message: %v", err)
	}

	if !slowPeer.closed.Load() {
		t.Fatal("expected slow peer to be closed when document update cannot be enqueued")
	}
	select {
	case <-slowPeer.Done():
	default:
		t.Fatal("expected slow peer done channel to be closed")
	}
	if got := currentDocumentContentForTest(t, store, documentID); got != "alpha bravo" {
		t.Fatalf("expected server to persist source update before closing slow peer, got %q", got)
	}
}

func TestDocumentRoomAwarenessBroadcastRemainsBestEffort(t *testing.T) {
	room := NewDocumentRooms().ForDocument("doc")
	slowPeer := newDocumentConn(1)
	slowPeer.send <- []byte("queued")
	room.Add(slowPeer)
	defer room.Remove(slowPeer)

	payload := yproto.BuildAwarenessUpdate(map[uint64]yproto.AwarenessState{
		1: {Clock: 1, State: []byte(`{"actorName":"a"}`)},
	}, []uint64{1})
	room.BroadcastBestEffort(payload, nil)

	if slowPeer.closed.Load() {
		t.Fatal("awareness overflow should not close the document session")
	}
	select {
	case <-slowPeer.Done():
		t.Fatal("awareness overflow should not close the done channel")
	default:
	}
}

func TestHandleDocumentProtocolMessageBroadcastsDeleteOnlySyncUpdate(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/backspace.md", "abc")

	peer := syncDocumentToDocForTest(t, store, documentID, 99)
	text := peer.GetText("content")

	room := server.rooms.ForDocument(documentID)
	source := &DocumentConn{send: make(chan []byte, 2)}
	peerConn := &DocumentConn{send: make(chan []byte, 2)}
	room.Add(source)
	room.Add(peerConn)
	defer room.Remove(source)
	defer room.Remove(peerConn)

	insertUpdate := captureDocUpdate(t, peer, "peer-insert", func(txn *crdt.Transaction) {
		text.Insert(txn, text.LenInTxn(txn), "d", nil)
	})
	if err := handleDocumentProtocolMessageForTest(t, server, store, room, source, documentID, yproto.BuildSyncUpdate(insertUpdate), OperationMeta{
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
		text.Delete(txn, text.LenInTxn(txn)-1, 1)
	})
	if err := handleDocumentProtocolMessageForTest(t, server, store, room, source, documentID, yproto.BuildSyncUpdate(deleteUpdate), OperationMeta{
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

	if got := currentDocumentContentForTest(t, store, documentID); got != "abc" {
		t.Fatalf("unexpected updated content after delete-only update: %q", got)
	}
}

func TestHandleDocumentProtocolMessageRespondsToSyncStep1(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "alpha bravo")

	source := &DocumentConn{send: make(chan []byte, 2)}
	err := handleDocumentProtocolMessageForTest(t, server, store, server.rooms.ForDocument(documentID), source, documentID, buildSyncStep1ForTest(crdt.New(crdt.WithClientID(77))), OperationMeta{
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

func TestHandleDocumentProtocolMessageIgnoresClosedSessionDuringSyncStep1(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/closed-session.md", "alpha bravo")
	room := server.rooms.ForDocument(documentID)
	source := newDocumentConn(0)
	room.Add(source)
	source.Close()
	room.Remove(source)

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("closed websocket session must not panic while sync replies are being built: %v", recovered)
		}
	}()
	if err := handleDocumentProtocolMessageForTest(t, server, store, room, source, documentID, buildSyncStep1ForTest(crdt.New(crdt.WithClientID(77))), OperationMeta{
		ActorID:   "peer",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("handle sync step1 for closed session: %v", err)
	}
}

func TestHandleDocumentProtocolMessageReconnectMergesServerAndClientEdits(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "base")

	clientDoc := syncDocumentToDocForTest(t, store, documentID, 101)
	serverPeerDoc := syncDocumentToDocForTest(t, store, documentID, 202)

	serverText := serverPeerDoc.GetText("content")
	serverUpdate := captureDocUpdate(t, serverPeerDoc, "server-peer", func(txn *crdt.Transaction) {
		serverText.Insert(txn, serverText.LenInTxn(txn), " server", nil)
	})
	room := server.rooms.ForDocument(documentID)
	serverPeer := &DocumentConn{send: make(chan []byte, 4)}
	if err := handleDocumentProtocolMessageForTest(t, server, store, room, serverPeer, documentID, yproto.BuildSyncUpdate(serverUpdate), OperationMeta{
		ActorID:   "server-peer",
		ActorType: "human",
		Source:    "test",
	}); err != nil {
		t.Fatalf("apply server-side edit: %v", err)
	}

	clientText := clientDoc.GetText("content")
	captureDocUpdate(t, clientDoc, "client-local", func(txn *crdt.Transaction) {
		clientText.Insert(txn, clientText.LenInTxn(txn), " client", nil)
	})

	reconnected := &DocumentConn{send: make(chan []byte, 4)}
	if err := handleDocumentProtocolMessageForTest(t, server, store, room, reconnected, documentID, buildSyncStep1ForTest(clientDoc), OperationMeta{
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
				if err := handleDocumentProtocolMessageForTest(t, server, store, room, reconnected, documentID, reply, OperationMeta{
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

	clientContent := clientDoc.GetText("content").ToString()
	currentContent := currentDocumentContentForTest(t, store, documentID)
	if currentContent != clientContent {
		t.Fatalf("expected server and client to converge, server=%q client=%q", currentContent, clientContent)
	}
	if !strings.Contains(currentContent, "server") || !strings.Contains(currentContent, "client") {
		t.Fatalf("expected converged content to include both edits, got %q", currentContent)
	}
}

func TestHandleDocumentProtocolMessageConcurrentSyncAndUpdates(t *testing.T) {
	server, store := newTestServer(t)
	documentID := mustCreateTestDocument(t, store, "docs/race.md", "base")
	room := server.rooms.ForDocument(documentID)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for worker := 0; worker < 4; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			peer := syncDocumentToDocForTest(t, store, documentID, uint64(300+worker))
			text := peer.GetText("content")
			conn := &DocumentConn{send: make(chan []byte, 1024)}
			for i := 0; i < 40; i++ {
				update := captureDocUpdate(t, peer, "writer", func(txn *crdt.Transaction) {
					text.Insert(txn, text.LenInTxn(txn), "x", nil)
				})
				if err := handleDocumentProtocolMessageForTest(t, server, store, room, conn, documentID, yproto.BuildSyncUpdate(update), OperationMeta{
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
				if err := handleDocumentProtocolMessageForTest(t, server, store, room, conn, documentID, buildSyncStep1ForTest(peer), OperationMeta{
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
	seedCodexDaemonRuntime(t, store)
	documentID := mustCreateTestDocument(t, store, "docs/spec.md", "Draft.\n")
	if _, err := store.CreateAgent(CreateAgentRequest{
		Handle: "codex-agent",
		Name:   "Codex Agent",
		Role:   "Reviews docs",
		Kind:   "codex",
	}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	frontendDoc := syncDocumentToDocForTest(t, store, documentID, 303)
	text := frontendDoc.GetText("content")
	update := captureDocUpdate(t, frontendDoc, "browser", func(txn *crdt.Transaction) {
		text.Insert(txn, text.LenInTxn(txn), "Ping @codex-agent.\n", nil)
	})

	events, unsubscribe := server.workspaceBroker(store.Snapshot().WorkspaceID).Subscribe()
	defer unsubscribe()

	source := &DocumentConn{send: make(chan []byte, 4)}
	if err := handleDocumentProtocolMessageForTest(t, server, store, server.rooms.ForDocument(documentID), source, documentID, yproto.BuildSyncUpdate(update), OperationMeta{
		ActorID:   "owner",
		ActorType: "human",
		Source:    "ws",
	}); err != nil {
		t.Fatalf("handle document protocol message: %v", err)
	}

	sawInboxChange := false
	for {
		select {
		case event := <-events:
			if event.Type == "document.mentions.updated" {
				t.Fatalf("document text mentions must not publish metadata change event: %#v", event)
			}
			if event.Type == "document.updated" {
				t.Fatalf("document update must not publish backend document namespace event: %#v", event)
			}
			if event.Type == "agent.inbox.changed" {
				sawInboxChange = true
			}
		default:
			if !sawInboxChange {
				t.Fatal("expected agent inbox change event to be published")
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
	reply, _, err := readSyncMessageForTest(reader, doc, origin)
	if err != nil {
		t.Fatalf("apply sync payload: %v", err)
	}
	return reply
}

func writeMuxRootUpdateForTest(t *testing.T, conn *websocket.Conn, rootID string, update []byte) {
	t.Helper()
	if len(update) == 0 {
		t.Fatal("root update must not be empty")
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, yproto.BuildDocumentMessage(rootID, yproto.BuildSyncUpdate(update))); err != nil {
		t.Fatalf("write mux root update: %v", err)
	}
}

func applyRawRootStreamUpdateForTest(t *testing.T, conn *websocket.Conn, doc *crdt.Doc) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set raw root read deadline: %v", err)
	}
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read raw root stream update: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("raw root stream message type = %d, want binary", messageType)
	}
	protocolType, reader, err := yproto.DecodeProtocolMessage(payload)
	if err != nil {
		t.Fatalf("decode raw root stream payload: %v", err)
	}
	if protocolType != yproto.MessageSync {
		t.Fatalf("raw root stream protocol = %d, want sync", protocolType)
	}
	syncType, data, err := yproto.DecodeSyncMessage(reader)
	if err != nil {
		t.Fatalf("decode raw root stream sync payload: %v", err)
	}
	if syncType != yproto.SyncUpdate {
		t.Fatalf("raw root stream sync type = %d, want update", syncType)
	}
	if err := crdt.ApplyUpdateV1(doc, data, "root-stream"); err != nil {
		t.Fatalf("apply raw root stream update: %v", err)
	}
}

func buildSyncStep1ForTest(doc *crdt.Doc) []byte {
	return yproto.BuildSyncStep1FromStateVector(crdt.EncodeStateVectorV1(doc))
}

func readSyncMessageForTest(reader *bytes.Reader, doc *crdt.Doc, origin any) ([]byte, bool, error) {
	syncType, data, err := yproto.DecodeSyncMessage(reader)
	if err != nil {
		return nil, false, err
	}
	switch syncType {
	case yproto.SyncStep1:
		update, err := doc.EncodeStateAsUpdateV1(data)
		if err != nil {
			return nil, false, err
		}
		return yproto.BuildSyncStep2FromUpdate(update), false, nil
	case yproto.SyncStep2, yproto.SyncUpdate:
		if err := crdt.ApplyUpdateV1(doc, data, origin); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	default:
		return nil, false, errors.New("unknown sync message type")
	}
}

func syncClientFromServer(t *testing.T, server *Server, store *Store, room *DocumentRoom, conn *DocumentConn, documentID string, doc *crdt.Doc) {
	t.Helper()
	if err := handleDocumentProtocolMessageForTest(t, server, store, room, conn, documentID, buildSyncStep1ForTest(doc), OperationMeta{
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
				if err := handleDocumentProtocolMessageForTest(t, server, store, room, conn, documentID, reply, OperationMeta{
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

func dialDocumentWebsocketForTest(t *testing.T, serverURL, path string, tokens ...string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + path
	if len(tokens) > 0 && strings.TrimSpace(tokens[0]) != "" {
		separator := "?"
		if strings.Contains(wsURL, "?") {
			separator = "&"
		}
		wsURL += separator + "token=" + url.QueryEscape(tokens[0])
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket %s: %v", path, err)
	}
	return conn
}

func readDocumentWebsocketFrameForTest(t *testing.T, conn *websocket.Conn) documentFrame {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set websocket read deadline: %v", err)
	}
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read websocket message: %v", err)
	}
	if messageType != websocket.BinaryMessage {
		t.Fatalf("websocket message type = %d, want binary", messageType)
	}
	documentID, inner, err := yproto.DecodeDocumentMessage(payload)
	if err != nil {
		t.Fatalf("decode routed document message: %v", err)
	}
	return documentFrame{documentID: documentID, payload: inner}
}

func rootEntryForDocumentForTest(t *testing.T, doc *crdt.Doc, documentID string) (string, bool) {
	t.Helper()
	root := doc.GetMap(rootMapName)
	var path string
	var deleted bool
	if err := doc.Read(func(txn *crdt.Transaction) error {
		entries, ok, err := root.GetMap(txn, rootEntriesMapName)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatalf("missing %s map", rootEntriesMapName)
		}
		entry, ok, err := entries.GetMap(txn, documentID)
		if err != nil {
			return err
		}
		if !ok {
			t.Fatalf("missing root entry for %s", documentID)
		}
		contentDocumentID, ok, err := entry.GetString(txn, "contentDocumentId")
		if err != nil {
			return err
		}
		if !ok || contentDocumentID != documentID {
			t.Fatalf("contentDocumentId = %q, want %q", contentDocumentID, documentID)
		}
		locValue, ok, err := entry.GetString(txn, "loc")
		if err != nil {
			return err
		}
		if !ok {
			t.Fatal("missing root entry loc")
		}
		var loc struct {
			ParentID string `json:"parentId"`
			Name     string `json:"name"`
		}
		if err := json.Unmarshal([]byte(locValue), &loc); err != nil {
			return err
		}
		path = loc.Name
		if loc.ParentID != "" {
			path = loc.ParentID + "/" + loc.Name
		}
		deletedValue, ok, err := entry.GetString(txn, "deleted")
		if err != nil {
			return err
		}
		deleted = ok && strings.EqualFold(deletedValue, "true")
		return nil
	}); err != nil {
		t.Fatalf("read root entry: %v", err)
	}
	return path, deleted
}

func rootEntryExistsForDocumentForTest(t *testing.T, doc *crdt.Doc, documentID string) bool {
	t.Helper()
	root := doc.GetMap(rootMapName)
	exists := false
	if err := doc.Read(func(txn *crdt.Transaction) error {
		entries, ok, err := root.GetMap(txn, rootEntriesMapName)
		if err != nil || !ok {
			return err
		}
		_, exists, err = entries.GetMap(txn, documentID)
		return err
	}); err != nil {
		t.Fatalf("read root entry existence: %v", err)
	}
	return exists
}

func upsertRootEntryForTest(doc *crdt.Doc, documentID, path string) ([]byte, error) {
	root := doc.GetMap(rootMapName)
	return doc.Update(func(txn *crdt.Transaction) error {
		entries, ok, err := root.GetMap(txn, rootEntriesMapName)
		if err != nil {
			return err
		}
		if !ok {
			entries, err = root.SetMap(txn, rootEntriesMapName)
			if err != nil {
				return err
			}
		}
		entry, ok, err := entries.GetMap(txn, documentID)
		if err != nil {
			return err
		}
		if !ok {
			entry, err = entries.SetMap(txn, documentID)
			if err != nil {
				return err
			}
		}
		loc, err := json.Marshal(map[string]string{"name": path})
		if err != nil {
			return err
		}
		if err := entry.SetString(txn, "kind", "file"); err != nil {
			return err
		}
		if err := entry.SetString(txn, "contentDocumentId", documentID); err != nil {
			return err
		}
		if err := entry.SetString(txn, "loc", string(loc)); err != nil {
			return err
		}
		return entry.SetString(txn, "deleted", "false")
	}, "root-stream-upsert")
}

func tombstoneRootEntryForTest(doc *crdt.Doc, documentID string) ([]byte, error) {
	root := doc.GetMap(rootMapName)
	return doc.Update(func(txn *crdt.Transaction) error {
		entries, ok, err := root.GetMap(txn, rootEntriesMapName)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("missing root entries map")
		}
		entry, ok, err := entries.GetMap(txn, documentID)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("missing root entry")
		}
		return entry.SetString(txn, "deleted", "true")
	}, "root-stream-tombstone")
}

func assertSyncPayloadForTest(t *testing.T, payload []byte) {
	t.Helper()
	messageType, _, err := yproto.DecodeProtocolMessage(payload)
	if err != nil {
		t.Fatalf("decode sync payload: %v", err)
	}
	if messageType != yproto.MessageSync {
		t.Fatalf("payload top-level = %d, want sync", messageType)
	}
}

func waitDocumentRoomSubscriberCount(t *testing.T, room *DocumentRoom, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		room.mu.Lock()
		got := len(room.conns)
		room.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	room.mu.Lock()
	got := len(room.conns)
	room.mu.Unlock()
	t.Fatalf("room subscriber count = %d, want %d", got, want)
}

func handleDocumentProtocolMessageForTest(t *testing.T, server *Server, store *Store, room *DocumentRoom, session documentSubscriber, documentID string, payload []byte, meta OperationMeta) error {
	t.Helper()
	if server == nil || store == nil {
		t.Fatal("server and store are required")
	}
	return server.handleDocumentProtocolMessageWithStore(store, server.workspaceBroker(store.Snapshot().WorkspaceID), room, session, documentID, payload, meta)
}

func newTestServer(t *testing.T) (*Server, *Store) {
	t.Helper()
	database := newPostgresTestDatabase(t)
	store := newPostgresTestWorkspaceStore(t, database)
	return NewServer(Config{}, database), store
}

type workspaceRouteTestFixture struct {
	server      *Server
	router      http.Handler
	store       *Store
	workspaceID string
	token       string
}

func newWorkspaceRouteTestFixture(t *testing.T) workspaceRouteTestFixture {
	t.Helper()
	server, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "route-owner@example.com", "owner-pass", "Route Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Route Test Tenant")
	store, err := server.workspaceStore(workspace.ID)
	if err != nil {
		t.Fatalf("open workspace store: %v", err)
	}
	return workspaceRouteTestFixture{
		server:      server,
		router:      router,
		store:       store,
		workspaceID: workspace.ID,
		token:       owner.Token,
	}
}

func (f workspaceRouteTestFixture) workspaceAPIPath(path string) string {
	return "/api/workspaces/" + f.workspaceID + path
}

func (f workspaceRouteTestFixture) workspaceWSPath(path string) string {
	return "/ws/workspaces/" + f.workspaceID + path
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
