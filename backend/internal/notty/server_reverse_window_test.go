package notty

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	crdt "notty/internal/ycrdt"
	"notty/internal/yproto"
)

func TestReverseWindowRoutesDeriveDaemonOriginAndBroadcastRootUpdates(t *testing.T) {
	server, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "reverse-route-owner@example.com", "owner-pass", "Reverse Route Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Reverse Route Tenant")
	store, err := server.workspaceStore(workspace.ID)
	if err != nil {
		t.Fatalf("open workspace store: %v", err)
	}
	var daemon CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Reverse daemon"}, http.StatusCreated, &daemon)

	contentDocumentID := mustCreateTestDocument(t, store, "docs/reverse.md", "server content\n")
	entryID := contentDocumentID
	const desiredPath = "docs/reverse.md"
	upsertReverseWindowRouteRootEntry(t, store, daemon.Daemon.ID, entryID, contentDocumentID, desiredPath)
	rootDoc := syncedReverseWindowRouteDocument(t, store, store.RootDocumentID())
	defer rootDoc.Close()
	rootRoom := server.rooms.ForDocument(workspace.ID + ":" + store.RootDocumentID())
	subscriber := newDocumentConn(4)
	rootRoom.Add(subscriber)
	defer rootRoom.Remove(subscriber)
	defer subscriber.Close()

	tombstoneOperationID := uuid.NewString()
	openBody := map[string]any{
		"originDaemonId":           uuid.NewString(),
		"originScope":              "agent:" + uuid.NewString(),
		"operationId":              tombstoneOperationID,
		"expectedWindowGeneration": 0,
		"entryId":                  entryID,
		"contentDocumentId":        contentDocumentID,
		"expectedDesiredPath":      `docs\reverse.md`,
	}
	var opened OpenReverseWindowResult
	authTestJSON(t, router, http.MethodPost, reverseWindowRoutePath(workspace.ID, "open"), daemon.Token, openBody, http.StatusOK, &opened)
	if opened.Outcome != OpenReverseWindowAcceptedNew || opened.WindowGeneration != 1 {
		t.Fatalf("open response = %#v, want accepted generation 1", opened)
	}
	var storedDaemonID, storedScope string
	if err := server.sqlDB().QueryRow(`SELECT origin_daemon_id::text, origin_scope
		FROM document_reverse_windows
		WHERE workspace_id = $1::uuid AND document_id = $2::uuid`, workspace.ID, contentDocumentID).Scan(&storedDaemonID, &storedScope); err != nil {
		t.Fatalf("read persisted reverse-window origin: %v", err)
	}
	if storedDaemonID != daemon.Daemon.ID || storedScope != "primary" {
		t.Fatalf("persisted origin = (%q, %q), want auth daemon (%q, primary)", storedDaemonID, storedScope, daemon.Daemon.ID)
	}
	applyReverseWindowRouteBroadcast(t, subscriber, rootDoc)
	if _, deleted := rootEntryForDocumentForTest(t, rootDoc, contentDocumentID); !deleted {
		t.Fatal("open route broadcast did not tombstone the root entry")
	}

	var replayed OpenReverseWindowResult
	authTestJSON(t, router, http.MethodPost, reverseWindowRoutePath(workspace.ID, "open"), daemon.Token, openBody, http.StatusOK, &replayed)
	if replayed.Outcome != OpenReverseWindowExactReplayStoredResult || replayed.WindowGeneration != opened.WindowGeneration {
		t.Fatalf("open replay response = %#v, want stored generation result", replayed)
	}
	assertNoReverseWindowRouteBroadcast(t, subscriber)
	conflictBody := make(map[string]any, len(openBody))
	for key, value := range openBody {
		conflictBody[key] = value
	}
	conflictBody["operationId"] = uuid.NewString()
	var conflict OpenReverseWindowResult
	authTestJSON(t, router, http.MethodPost, reverseWindowRoutePath(workspace.ID, "open"), daemon.Token, conflictBody, http.StatusOK, &conflict)
	if conflict.Outcome != OpenReverseWindowGenerationConflict || conflict.CurrentWindowGeneration != 1 {
		t.Fatalf("generation-conflict response = %#v, want current generation 1", conflict)
	}
	assertNoReverseWindowRouteBroadcast(t, subscriber)

	restoreOperationID := uuid.NewString()
	consumeBody := map[string]any{
		"originDaemonId":       uuid.NewString(),
		"originScope":          "agent:" + uuid.NewString(),
		"tombstoneOperationId": tombstoneOperationID,
		"restoreOperationId":   restoreOperationID,
		"windowGeneration":     1,
		"contentStateVector":   syncedReverseWindowRouteStateVector(t, store, contentDocumentID),
	}
	var consumed ConsumeReverseWindowResult
	authTestJSON(t, router, http.MethodPost, reverseWindowRoutePath(workspace.ID, "consume"), daemon.Token, consumeBody, http.StatusOK, &consumed)
	if consumed.Outcome != ConsumeReverseWindowAccepted || consumed.WindowGeneration != 1 {
		t.Fatalf("consume response = %#v, want accepted generation 1", consumed)
	}
	applyReverseWindowRouteBroadcast(t, subscriber, rootDoc)
	if _, deleted := rootEntryForDocumentForTest(t, rootDoc, contentDocumentID); deleted {
		t.Fatal("consume route broadcast did not reactivate the root entry")
	}
	consumeBody["contentStateVector"] = nil
	var consumeReplay ConsumeReverseWindowResult
	authTestJSON(t, router, http.MethodPost, reverseWindowRoutePath(workspace.ID, "consume"), daemon.Token, consumeBody, http.StatusOK, &consumeReplay)
	if consumeReplay.Outcome != ConsumeReverseWindowExactReplayStoredResult || consumeReplay.RestoreUpdateID != consumed.RestoreUpdateID {
		t.Fatalf("consume replay response = %#v, want stored restore result %#v", consumeReplay, consumed)
	}
	assertNoReverseWindowRouteBroadcast(t, subscriber)
}

func TestReverseWindowRoutesRejectHumansAndDeriveActingAgentOrigin(t *testing.T) {
	server, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "reverse-agent-owner@example.com", "owner-pass", "Reverse Agent Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Reverse Agent Tenant")
	store, err := server.workspaceStore(workspace.ID)
	if err != nil {
		t.Fatalf("open workspace store: %v", err)
	}
	var daemon CreateDaemonResponse
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons", owner.Token, CreateDaemonRequest{Name: "Agent daemon"}, http.StatusCreated, &daemon)
	authTestReportCodexRuntime(t, router, workspace.ID, daemon.Token)
	var agent Agent
	authTestJSON(t, router, http.MethodPost, "/api/workspaces/"+workspace.ID+"/daemons/"+daemon.Daemon.ID+"/agents", owner.Token, CreateAgentRequest{
		Handle: "reverse-agent", Name: "Reverse Agent", Role: "restore files", Kind: "codex",
	}, http.StatusCreated, &agent)

	contentDocumentID := mustCreateTestDocument(t, store, "docs/agent.md", "agent content\n")
	entryID := contentDocumentID
	upsertReverseWindowRouteRootEntry(t, store, daemon.Daemon.ID, entryID, contentDocumentID, "docs/agent.md")
	body := map[string]any{
		"originDaemonId":           uuid.NewString(),
		"originScope":              "primary",
		"operationId":              uuid.NewString(),
		"expectedWindowGeneration": 0,
		"entryId":                  entryID,
		"contentDocumentId":        contentDocumentID,
		"expectedDesiredPath":      "docs/agent.md",
	}
	authTestStatus(t, router, http.MethodPost, reverseWindowRoutePath(workspace.ID, "open"), owner.Token, body, http.StatusForbidden)

	var opened OpenReverseWindowResult
	authTestJSONWithHeaders(t, router, http.MethodPost, reverseWindowRoutePath(workspace.ID, "open"), daemon.Token, map[string]string{
		"X-Notty-Acting-Agent-ID": agent.ID,
	}, body, http.StatusOK, &opened)
	if opened.Outcome != OpenReverseWindowAcceptedNew {
		t.Fatalf("acting-agent open response = %#v", opened)
	}
	var storedDaemonID, storedScope string
	if err := server.sqlDB().QueryRow(`SELECT origin_daemon_id::text, origin_scope
		FROM document_reverse_windows
		WHERE workspace_id = $1::uuid AND document_id = $2::uuid`, workspace.ID, contentDocumentID).Scan(&storedDaemonID, &storedScope); err != nil {
		t.Fatalf("read acting-agent origin: %v", err)
	}
	wantScope := "agent:" + agent.ID
	if storedDaemonID != daemon.Daemon.ID || storedScope != wantScope {
		t.Fatalf("persisted acting-agent origin = (%q, %q), want (%q, %q)", storedDaemonID, storedScope, daemon.Daemon.ID, wantScope)
	}
}

func reverseWindowRoutePath(workspaceID, action string) string {
	return "/api/workspaces/" + workspaceID + "/documents/reverse-window/" + action
}

func upsertReverseWindowRouteRootEntry(t *testing.T, store *Store, daemonID, entryID, contentDocumentID, path string) {
	t.Helper()
	rootDocument, err := store.GetDocument(store.RootDocumentID())
	if err != nil {
		t.Fatalf("get root document: %v", err)
	}
	doc, err := store.restoreDocumentDocPostgresLocked(rootDocument)
	if err != nil {
		t.Fatalf("restore root document: %v", err)
	}
	defer doc.Close()
	update, err := upsertRootCommandEntryForTest(doc, entryID, contentDocumentID, path)
	if err != nil {
		t.Fatalf("build root entry: %v", err)
	}
	if _, err := store.ApplyCRDTUpdate(store.RootDocumentID(), update, OperationMeta{
		ActorID: daemonID, ActorType: "daemon", Source: "reverse-window-route-test",
	}); err != nil {
		t.Fatalf("apply root entry: %v", err)
	}
}

func syncedReverseWindowRouteDocument(t *testing.T, store *Store, documentID string) *crdt.Doc {
	t.Helper()
	doc := crdt.New()
	_, updates, err := store.EncodeDocumentSyncUpdates(documentID, nil)
	if err != nil {
		doc.Close()
		t.Fatalf("sync document %s: %v", documentID, err)
	}
	for _, update := range updates {
		if err := crdt.ApplyUpdateV1(doc, update, "reverse-window-route-bootstrap"); err != nil {
			doc.Close()
			t.Fatalf("apply document %s bootstrap: %v", documentID, err)
		}
	}
	return doc
}

func syncedReverseWindowRouteStateVector(t *testing.T, store *Store, documentID string) []byte {
	t.Helper()
	doc := syncedReverseWindowRouteDocument(t, store, documentID)
	defer doc.Close()
	return crdt.EncodeStateVectorV1(doc)
}

func applyReverseWindowRouteBroadcast(t *testing.T, subscriber *DocumentConn, doc *crdt.Doc) {
	t.Helper()
	var payload []byte
	select {
	case payload = <-subscriber.send:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for root-document broadcast")
	}
	messageType, reader, err := yproto.DecodeProtocolMessage(payload)
	if err != nil {
		t.Fatalf("decode root broadcast: %v", err)
	}
	if messageType != yproto.MessageSync {
		t.Fatalf("root broadcast message type = %d, want sync", messageType)
	}
	syncType, update, err := yproto.DecodeSyncMessage(reader)
	if err != nil {
		t.Fatalf("decode root broadcast sync message: %v", err)
	}
	if syncType != yproto.SyncUpdate {
		t.Fatalf("root broadcast sync type = %d, want update", syncType)
	}
	if err := crdt.ApplyUpdateV1(doc, update, "reverse-window-route-broadcast"); err != nil {
		t.Fatalf("apply root broadcast: %v", err)
	}
}

func assertNoReverseWindowRouteBroadcast(t *testing.T, subscriber *DocumentConn) {
	t.Helper()
	select {
	case payload := <-subscriber.send:
		t.Fatalf("mutation-free replay broadcast %d bytes", len(payload))
	default:
	}
}
