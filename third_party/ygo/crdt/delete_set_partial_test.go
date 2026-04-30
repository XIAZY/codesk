package crdt

import "testing"

func TestApplyDeleteSetSplitsPartiallyCoveredTextItem(t *testing.T) {
	source := New(WithClientID(1))
	sourceText := source.GetText("content")
	source.Transact(func(txn *Transaction) {
		sourceText.Insert(txn, 0, "abcd", nil)
	}, "seed")

	writer := New(WithClientID(2))
	viewer := New(WithClientID(3))
	initial := EncodeStateAsUpdateV1(source, nil)
	if err := ApplyUpdateV1(writer, initial, "initial"); err != nil {
		t.Fatalf("sync writer: %v", err)
	}
	if err := ApplyUpdateV1(viewer, initial, "initial"); err != nil {
		t.Fatalf("sync viewer: %v", err)
	}

	writerText := writer.GetText("content")
	var deleteUpdate []byte
	unsubscribe := writer.OnUpdate(func(next []byte, origin any) {
		if origin == "delete" {
			deleteUpdate = append([]byte(nil), next...)
		}
	})
	writer.Transact(func(txn *Transaction) {
		writerText.Delete(txn, writerText.Len()-1, 1)
	}, "delete")
	unsubscribe()

	if got := writerText.ToString(); got != "abc" {
		t.Fatalf("unexpected writer content after delete: %q", got)
	}
	if err := ApplyUpdateV1(viewer, deleteUpdate, "remote-delete"); err != nil {
		t.Fatalf("apply delete update: %v", err)
	}
	if got := viewer.GetText("content").ToString(); got != "abc" {
		t.Fatalf("unexpected viewer content after delete: %q", got)
	}
}
