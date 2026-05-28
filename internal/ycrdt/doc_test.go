package ycrdt

import (
	"encoding/hex"
	"errors"
	"testing"
)

func TestYCRDTRoundTripUpdate(t *testing.T) {
	source := New(WithClientID(1))
	defer source.Close()
	target := New(WithClientID(2))
	defer target.Close()
	text := source.GetText("content")

	update, err := source.Update(func(txn *Transaction) error {
		text.Insert(txn, 0, "hello", nil)
		return nil
	}, "source")
	if err != nil {
		t.Fatalf("update source: %v", err)
	}
	if err := ApplyUpdateV1(target, update, "remote"); err != nil {
		t.Fatalf("apply update: %v", err)
	}
	if got := target.GetText("content").ToString(); got != "hello" {
		t.Fatalf("got %q want hello", got)
	}
}

func TestYCRDTStateVectorDiff(t *testing.T) {
	source := New(WithClientID(1))
	defer source.Close()
	target := New(WithClientID(2))
	defer target.Close()
	text := source.GetText("content")

	_, err := source.Update(func(txn *Transaction) error {
		text.Insert(txn, 0, "alpha", nil)
		return nil
	}, "source")
	if err != nil {
		t.Fatalf("update source: %v", err)
	}
	stateVector := EncodeStateVectorV1(target)
	update, err := source.EncodeStateAsUpdateV1(stateVector)
	if err != nil {
		t.Fatalf("encode diff: %v", err)
	}
	if err := ApplyUpdateV1(target, update, "remote"); err != nil {
		t.Fatalf("apply diff: %v", err)
	}
	if got := target.GetText("content").ToString(); got != "alpha" {
		t.Fatalf("got %q want alpha", got)
	}
}

func TestYCRDTDeleteUpdate(t *testing.T) {
	doc := New(WithClientID(1))
	defer doc.Close()
	viewer := New(WithClientID(2))
	defer viewer.Close()
	text := doc.GetText("content")

	initial, err := doc.Update(func(txn *Transaction) error {
		text.Insert(txn, 0, "abcd", nil)
		return nil
	}, "initial")
	if err != nil {
		t.Fatalf("initial update: %v", err)
	}
	if err := ApplyUpdateV1(viewer, initial, "initial"); err != nil {
		t.Fatalf("apply initial: %v", err)
	}
	deleteUpdate, err := doc.Update(func(txn *Transaction) error {
		text.Delete(txn, 1, 2)
		return nil
	}, "delete")
	if err != nil {
		t.Fatalf("delete update: %v", err)
	}
	if err := ApplyUpdateV1(viewer, deleteUpdate, "remote-delete"); err != nil {
		t.Fatalf("apply delete: %v", err)
	}
	if got := viewer.GetText("content").ToString(); got != "ad" {
		t.Fatalf("got %q want ad", got)
	}
}

func TestYCRDTKnownRightOriginCase(t *testing.T) {
	server := New(WithClientID(1))
	defer server.Close()
	writer := New(WithClientID(2))
	defer writer.Close()
	viewer := New(WithClientID(3))
	defer viewer.Close()
	serverText := server.GetText("content")
	writerText := writer.GetText("content")

	base, err := server.Update(func(txn *Transaction) error {
		serverText.Insert(txn, 0, "ab", nil)
		return nil
	}, "base")
	if err != nil {
		t.Fatalf("base update: %v", err)
	}
	if err := ApplyUpdateV1(writer, base, "base"); err != nil {
		t.Fatalf("writer apply base: %v", err)
	}
	if err := ApplyUpdateV1(viewer, base, "base"); err != nil {
		t.Fatalf("viewer apply base: %v", err)
	}
	update, err := writer.Update(func(txn *Transaction) error {
		writerText.Insert(txn, 1, "X", nil)
		return nil
	}, "insert-middle")
	if err != nil {
		t.Fatalf("middle insert update: %v", err)
	}
	if err := ApplyUpdateV1(viewer, update, "remote"); err != nil {
		t.Fatalf("viewer apply middle insert: %v", err)
	}
	if got := viewer.GetText("content").ToString(); got != "aXb" {
		t.Fatalf("got %q want aXb", got)
	}
}

func TestYCRDTAppliesYjsRightOriginUpdate(t *testing.T) {
	doc := New()
	defer doc.Close()

	for _, encoded := range []string{
		"01010100040107636f6e74656e7402616200",
		"01010200c401000101015800",
	} {
		update, err := hex.DecodeString(encoded)
		if err != nil {
			t.Fatalf("decode update: %v", err)
		}
		if err := ApplyUpdateV1(doc, update, "yjs"); err != nil {
			t.Fatalf("apply yjs update: %v", err)
		}
	}
	if got := doc.GetText("content").ToString(); got != "aXb" {
		t.Fatalf("got %q want aXb", got)
	}
}

func TestYCRDTUTF16Offsets(t *testing.T) {
	doc := New(WithClientID(1))
	defer doc.Close()
	text := doc.GetText("content")

	_, err := doc.Update(func(txn *Transaction) error {
		text.Insert(txn, 0, "a🙂b", nil)
		text.Insert(txn, 3, "X", nil)
		return nil
	}, "utf16")
	if err != nil {
		t.Fatalf("utf16 update: %v", err)
	}
	if got := doc.GetText("content").ToString(); got != "a🙂Xb" {
		t.Fatalf("got %q want a🙂Xb", got)
	}
}

func TestYTextInsertValueRejectsInvalidUTF8(t *testing.T) {
	doc := New(WithClientID(1))
	defer doc.Close()
	text := doc.GetText("content")

	_, err := doc.Update(func(txn *Transaction) error {
		return text.InsertValue(txn, 0, string([]byte{0xff}))
	}, "invalid")
	if !errors.Is(err, ErrInvalidYTextString) {
		t.Fatalf("expected invalid ytext string error, got %v", err)
	}
	if got := text.ToString(); got != "" {
		t.Fatalf("invalid insert should not change text, got %q", got)
	}
}

func TestYCRDTLenInTxnSeesTransactionChanges(t *testing.T) {
	doc := New(WithClientID(1))
	defer doc.Close()
	text := doc.GetText("content")

	_, err := doc.Update(func(txn *Transaction) error {
		text.Insert(txn, 0, "a", nil)
		text.Insert(txn, text.LenInTxn(txn), "b", nil)
		return nil
	}, "append")
	if err != nil {
		t.Fatalf("update doc: %v", err)
	}
	if got := text.ToString(); got != "ab" {
		t.Fatalf("got %q want ab", got)
	}
}
