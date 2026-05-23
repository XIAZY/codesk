package ycrdt

import (
	"encoding/json"
	"testing"
)

func TestDocGUIDOptionAndYMapJSONRoundTrip(t *testing.T) {
	doc := New(WithGUID("root_stream_test"))
	if got := doc.GUID(); got != "root_stream_test" {
		t.Fatalf("expected configured guid, got %q", got)
	}

	entries := doc.GetMap("entriesById")
	update, err := doc.Update(func(txn *Transaction) error {
		if err := entries.InsertJSON(txn, "doc_1", `{"id":"doc_1","kind":"file","contentStreamId":"doc_1"}`); err != nil {
			return err
		}
		if err := entries.InsertString(txn, "marker", "ready"); err != nil {
			return err
		}
		return nil
	}, "map-test")
	if err != nil {
		t.Fatalf("update map: %v", err)
	}
	if len(update) == 0 {
		t.Fatal("expected map update bytes")
	}
	if entries.Len() != 2 {
		t.Fatalf("expected two entries, got %d", entries.Len())
	}
	mapJSON, err := entries.JSON()
	if err != nil {
		t.Fatalf("map json: %v", err)
	}
	var decodedMap map[string]any
	if err := json.Unmarshal([]byte(mapJSON), &decodedMap); err != nil {
		t.Fatalf("decode map json %q: %v", mapJSON, err)
	}
	if _, ok := decodedMap["doc_1"].(map[string]any); !ok {
		t.Fatalf("expected doc_1 object in map json, got %#v", decodedMap["doc_1"])
	}
	raw, ok, err := entries.GetJSON("doc_1")
	if err != nil || !ok {
		t.Fatalf("get json entry ok=%t err=%v", ok, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("decode json entry %q: %v", raw, err)
	}
	if decoded["id"] != "doc_1" || decoded["kind"] != "file" {
		t.Fatalf("unexpected decoded entry %#v", decoded)
	}
	marker, ok, err := entries.GetJSON("marker")
	if err != nil || !ok || marker != `"ready"` {
		t.Fatalf("unexpected marker json=%q ok=%t err=%v", marker, ok, err)
	}

	peer := New()
	if err := ApplyUpdateV1(peer, update, "peer"); err != nil {
		t.Fatalf("apply map update: %v", err)
	}
	peerRaw, ok, err := peer.GetMap("entriesById").GetJSON("doc_1")
	if err != nil || !ok {
		t.Fatalf("peer map mismatch raw=%q ok=%t err=%v", peerRaw, ok, err)
	}
	var peerDecoded map[string]any
	if err := json.Unmarshal([]byte(peerRaw), &peerDecoded); err != nil {
		t.Fatalf("decode peer json entry %q: %v", peerRaw, err)
	}
	if peerDecoded["id"] != decoded["id"] || peerDecoded["kind"] != decoded["kind"] {
		t.Fatalf("unexpected peer decoded entry %#v", peerDecoded)
	}
}
