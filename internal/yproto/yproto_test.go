package yproto

import (
	"testing"

	"github.com/reearth/ygo/crdt"
)

func TestReadSyncMessageAppliesIncrementalUpdate(t *testing.T) {
	source := crdt.New(crdt.WithClientID(1))
	target := crdt.New(crdt.WithClientID(2))

	text := source.GetText("content")
	var update []byte
	unsubscribe := source.OnUpdate(func(next []byte, origin any) {
		update = append([]byte(nil), next...)
	})
	source.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, 0, "hello", nil)
	}, "local")
	unsubscribe()

	payload := BuildSyncUpdate(update)
	topLevel, reader, err := DecodeProtocolMessage(payload)
	if err != nil {
		t.Fatalf("decode protocol message: %v", err)
	}
	if topLevel != MessageSync {
		t.Fatalf("unexpected top-level message: %d", topLevel)
	}
	reply, changed, err := ReadSyncMessage(reader, target, "remote")
	if err != nil {
		t.Fatalf("read sync message: %v", err)
	}
	if len(reply) != 0 {
		t.Fatalf("expected no reply for sync update, got %d bytes", len(reply))
	}
	if !changed {
		t.Fatal("expected sync update to change target doc")
	}
	if got := target.GetText("content").ToString(); got != "hello" {
		t.Fatalf("unexpected target content: %q", got)
	}
}

func TestDeleteUpdateAfterSyncOnlyDeletesTargetRange(t *testing.T) {
	server := crdt.New(crdt.WithClientID(1))
	serverText := server.GetText("content")
	server.Transact(func(txn *crdt.Transaction) {
		serverText.Insert(txn, 0, "abcd", nil)
	}, "seed")

	writer := crdt.New(crdt.WithClientID(2))
	viewer := crdt.New(crdt.WithClientID(3))
	initial := crdt.EncodeStateAsUpdateV1(server, nil)
	if err := crdt.ApplyUpdateV1(writer, initial, "initial"); err != nil {
		t.Fatalf("sync writer: %v", err)
	}
	if err := crdt.ApplyUpdateV1(viewer, initial, "initial"); err != nil {
		t.Fatalf("sync viewer: %v", err)
	}

	writerText := writer.GetText("content")
	var deleteUpdate []byte
	unsubscribe := writer.OnUpdate(func(next []byte, origin any) {
		if origin == "delete" {
			deleteUpdate = append([]byte(nil), next...)
		}
	})
	writer.Transact(func(txn *crdt.Transaction) {
		writerText.Delete(txn, writerText.Len()-1, 1)
	}, "delete")
	unsubscribe()

	if got := writerText.ToString(); got != "abc" {
		t.Fatalf("unexpected writer content after delete: %q", got)
	}
	if err := crdt.ApplyUpdateV1(viewer, deleteUpdate, "remote-delete"); err != nil {
		t.Fatalf("apply delete update: %v", err)
	}
	if got := viewer.GetText("content").ToString(); got != "abc" {
		t.Fatalf("unexpected viewer content after delete: %q", got)
	}
}

func TestBuildAndDecodeAwarenessUpdateRoundTrip(t *testing.T) {
	payload := BuildAwarenessUpdate(map[uint64]AwarenessState{
		7: {Clock: 2, State: []byte(`{"actorId":"owner"}`)},
	}, []uint64{7})

	topLevel, reader, err := DecodeProtocolMessage(payload)
	if err != nil {
		t.Fatalf("decode protocol message: %v", err)
	}
	if topLevel != MessageAwareness {
		t.Fatalf("unexpected top-level message: %d", topLevel)
	}
	states, err := DecodeAwarenessUpdate(reader)
	if err != nil {
		t.Fatalf("decode awareness update: %v", err)
	}
	state, ok := states[7]
	if !ok {
		t.Fatal("expected client 7 awareness state")
	}
	if state.Clock != 2 {
		t.Fatalf("unexpected awareness clock: %d", state.Clock)
	}
	if got := string(state.State); got != `{"actorId":"owner"}` {
		t.Fatalf("unexpected awareness payload: %q", got)
	}
}
