package syncer

import (
	"bytes"
	"context"
	"testing"
	"time"

	crdt "notty/internal/ycrdt"
	"notty/internal/yproto"
)

func TestDocumentSocketStateVectorWaitRegistersBeforeProbe(t *testing.T) {
	socket := newWorkspaceDocumentSocket(nil)
	doc := crdt.New()
	t.Cleanup(doc.Close)
	insertStateVectorTestText(t, doc, 0, "required")
	required := crdt.EncodeStateVectorV1(doc)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- socket.WaitForBackendStateVector(ctx, "doc-1", required)
	}()

	var probe outboundDocumentMessage
	select {
	case probe = <-socket.send:
	case <-ctx.Done():
		t.Fatal("state-vector probe was not queued")
	}
	if probe.documentID != "doc-1" {
		t.Fatalf("probe document = %q, want doc-1", probe.documentID)
	}
	if want := yproto.BuildSyncStep1FromStateVector(required); !bytes.Equal(probe.payload, want) {
		t.Fatalf("probe payload = %x, want %x", probe.payload, want)
	}
	if err := socket.observeBackendStateVector("doc-1", required); err != nil {
		t.Fatalf("observe immediate backend response: %v", err)
	}
	probe.result <- nil
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("wait for backend state vector: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("registered waiter missed response racing the probe write")
	}
}

func TestDocumentSocketStateVectorWaiterRequiresDominance(t *testing.T) {
	socket := newWorkspaceDocumentSocket(nil)
	requiredDoc := crdt.New()
	t.Cleanup(requiredDoc.Close)
	insertStateVectorTestText(t, requiredDoc, 0, "required")
	required := crdt.EncodeStateVectorV1(requiredDoc)
	wait, cancel, err := socket.registerStateVectorWaiter("doc-1", required)
	if err != nil {
		t.Fatalf("register state-vector waiter: %v", err)
	}
	defer cancel()

	behindDoc := crdt.New()
	t.Cleanup(behindDoc.Close)
	if err := socket.observeBackendStateVector("doc-1", crdt.EncodeStateVectorV1(behindDoc)); err != nil {
		t.Fatalf("observe behind frontier: %v", err)
	}
	select {
	case <-wait:
		t.Fatal("behind backend frontier released the waiter")
	default:
	}

	aheadDoc := crdt.New()
	t.Cleanup(aheadDoc.Close)
	if err := crdt.ApplyUpdateV1(aheadDoc, requiredDoc.EncodeStateAsUpdate(), "required-copy"); err != nil {
		t.Fatalf("copy required state: %v", err)
	}
	insertStateVectorTestText(t, aheadDoc, len("required"), " and ahead")
	if err := socket.observeBackendStateVector("doc-1", crdt.EncodeStateVectorV1(aheadDoc)); err != nil {
		t.Fatalf("observe dominating frontier: %v", err)
	}
	select {
	case <-wait:
	case <-time.After(time.Second):
		t.Fatal("dominating backend frontier did not release the waiter")
	}
}

func TestDocumentSocketStateVectorWaiterRejectsMalformedAndEmptyVectors(t *testing.T) {
	socket := newWorkspaceDocumentSocket(nil)
	if _, _, err := socket.registerStateVectorWaiter("doc-1", nil); err == nil {
		t.Fatal("empty required frontier was accepted")
	}
	doc := crdt.New()
	t.Cleanup(doc.Close)
	insertStateVectorTestText(t, doc, 0, "required")
	wait, cancel, err := socket.registerStateVectorWaiter("doc-1", crdt.EncodeStateVectorV1(doc))
	if err != nil {
		t.Fatalf("register state-vector waiter: %v", err)
	}
	defer cancel()
	if err := socket.observeBackendStateVector("doc-1", []byte{0xff}); err == nil {
		t.Fatal("malformed backend frontier was accepted")
	}
	select {
	case <-wait:
		t.Fatal("malformed backend frontier released the waiter")
	default:
	}
}

func insertStateVectorTestText(t *testing.T, doc *crdt.Doc, index int, value string) {
	t.Helper()
	text := doc.GetText("content")
	if _, err := doc.Update(func(txn *crdt.Transaction) error {
		text.Insert(txn, index, value, nil)
		return nil
	}, "test"); err != nil {
		t.Fatalf("insert state-vector test text: %v", err)
	}
}
