package syncer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/reearth/ygo/crdt"
)

func TestDocumentCacheMaterializesViaStateVectorDeltas(t *testing.T) {
	serverDoc := newDocWithText(t, "alpha")
	var requestedVectors []string

	client := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method != http.MethodPost || r.URL.Path != "/api/documents/doc_1/sync" {
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
			var req documentSyncRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode sync request: %v", err)
			}
			requestedVectors = append(requestedVectors, req.StateVector)
			var stateVector crdt.StateVector
			if req.StateVector != "" {
				decoded, err := base64.StdEncoding.DecodeString(req.StateVector)
				if err != nil {
					t.Fatalf("decode state vector: %v", err)
				}
				stateVector, err = crdt.DecodeStateVectorV1(decoded)
				if err != nil {
					t.Fatalf("parse state vector: %v", err)
				}
			}
			update := crdt.EncodeStateAsUpdateV1(serverDoc, stateVector)
			response := documentSyncResponse{
				Document: &document{ID: "doc_1", Path: "docs/spec.md", UpdateID: int64(len(requestedVectors))},
				Update:   base64.StdEncoding.EncodeToString(update),
			}
			body, err := json.Marshal(response)
			if err != nil {
				t.Fatalf("marshal response: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(body)),
				Header:     http.Header{"Content-Type": []string{"application/json"}},
			}, nil
		}),
	}

	cache, err := newDocumentCache(t.TempDir(), "http://backend.test", client)
	if err != nil {
		t.Fatalf("new cache: %v", err)
	}
	first, err := cache.materialize(context.Background(), &document{ID: "doc_1", Path: "docs/spec.md"})
	if err != nil {
		t.Fatalf("first materialize: %v", err)
	}
	if first.Content != "alpha" {
		t.Fatalf("unexpected first content: %q", first.Content)
	}

	text := serverDoc.GetText("content")
	serverDoc.Transact(func(txn *crdt.Transaction) {
		text.Insert(txn, text.Len(), " beta", nil)
	}, "server")

	second, err := cache.materialize(context.Background(), &document{ID: "doc_1", Path: "docs/spec.md"})
	if err != nil {
		t.Fatalf("second materialize: %v", err)
	}
	if second.Content != "alpha beta" {
		t.Fatalf("unexpected second content: %q", second.Content)
	}
	if len(requestedVectors) != 2 {
		t.Fatalf("expected two document sync requests, got %d", len(requestedVectors))
	}
	if requestedVectors[1] == "" {
		t.Fatal("expected second sync to use cached state vector")
	}
	if _, err := os.Stat(cache.statePath("doc_1")); err != nil {
		t.Fatalf("expected cache state on disk: %v", err)
	}
}
