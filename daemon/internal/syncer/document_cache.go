package syncer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/reearth/ygo/crdt"
)

type documentCache struct {
	root       string
	backendURL string
	client     *http.Client

	mu      sync.Mutex
	entries map[string]*documentCacheEntry
}

type documentCacheEntry struct {
	mu       sync.Mutex
	doc      *crdt.Doc
	metadata documentCacheMetadata
	loaded   bool
}

type documentCacheMetadata struct {
	DocumentID  string    `json:"documentId"`
	Path        string    `json:"path"`
	UpdateID    int64     `json:"updateId,omitempty"`
	StateVector string    `json:"stateVector,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type materializedCachedDocument struct {
	Doc      *crdt.Doc
	DocMu    *sync.Mutex
	Entry    *documentCacheEntry
	Content  string
	UpdateID int64
}

type documentSyncRequest struct {
	StateVector string `json:"stateVector,omitempty"`
}

type documentSyncResponse struct {
	Document *document `json:"document,omitempty"`
	Update   string    `json:"update,omitempty"`
}

func newDocumentCache(root, backendURL string, client *http.Client) (*documentCache, error) {
	if strings.TrimSpace(root) == "" {
		return nil, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &documentCache{
		root:       root,
		backendURL: strings.TrimRight(backendURL, "/"),
		client:     client,
		entries:    map[string]*documentCacheEntry{},
	}, nil
}

func (c *documentCache) materialize(ctx context.Context, meta *document) (*materializedCachedDocument, error) {
	if meta == nil {
		return nil, errors.New("document metadata is required")
	}
	if c == nil {
		doc, err := decodeDoc(meta.CRDTState)
		if err != nil {
			return nil, err
		}
		return &materializedCachedDocument{
			Doc:      doc,
			DocMu:    &sync.Mutex{},
			Content:  doc.GetText("content").ToString(),
			UpdateID: meta.UpdateID,
		}, nil
	}

	entry := c.entryFor(meta.ID)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	if !entry.loaded {
		doc, metadata, err := c.loadLocked(meta)
		if err != nil {
			return nil, err
		}
		entry.doc = doc
		entry.metadata = metadata
		entry.loaded = true
	}
	if entry.doc == nil {
		entry.doc = crdt.New()
	}
	stateVector := entry.metadata.StateVector
	if stateVector == "" {
		stateVector = base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(entry.doc))
	}
	synced, err := c.fetchMissingUpdate(ctx, meta.ID, stateVector)
	if err != nil {
		return nil, err
	}
	if synced.Document != nil {
		entry.metadata.Path = synced.Document.Path
		entry.metadata.UpdateID = synced.Document.UpdateID
	}
	if synced.Update != "" {
		update, err := base64.StdEncoding.DecodeString(synced.Update)
		if err != nil {
			return nil, err
		}
		if len(update) > 0 {
			if err := crdt.ApplyUpdateV1(entry.doc, update, "cache-sync"); err != nil {
				return nil, err
			}
		}
	}

	entry.metadata.DocumentID = meta.ID
	if entry.metadata.Path == "" {
		entry.metadata.Path = meta.Path
	}
	if entry.metadata.UpdateID == 0 {
		entry.metadata.UpdateID = meta.UpdateID
	}
	entry.metadata.StateVector = base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(entry.doc))
	entry.metadata.UpdatedAt = time.Now().UTC()
	if err := c.storeDocLocked(entry.metadata.DocumentID, entry.metadata.Path, entry.metadata.UpdateID, entry.doc); err != nil {
		return nil, err
	}
	return &materializedCachedDocument{
		Doc:      entry.doc,
		DocMu:    &entry.mu,
		Entry:    entry,
		Content:  entry.doc.GetText("content").ToString(),
		UpdateID: entry.metadata.UpdateID,
	}, nil
}

func (c *documentCache) storeDoc(documentID, path string, updateID int64, doc *crdt.Doc) error {
	if c == nil || documentID == "" || doc == nil {
		return nil
	}
	entry := c.entryFor(documentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	entry.doc = doc
	entry.metadata = documentCacheMetadata{
		DocumentID:  documentID,
		Path:        path,
		UpdateID:    updateID,
		StateVector: base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc)),
		UpdatedAt:   time.Now().UTC(),
	}
	entry.loaded = true
	return c.storeDocLocked(documentID, path, updateID, doc)
}

func (c *documentCache) storeDocLocked(documentID, path string, updateID int64, doc *crdt.Doc) error {
	if err := os.MkdirAll(c.documentDir(documentID), 0o755); err != nil {
		return err
	}
	state := doc.EncodeStateAsUpdate()
	if err := os.WriteFile(c.statePath(documentID), state, 0o644); err != nil {
		return err
	}
	metadata := documentCacheMetadata{
		DocumentID:  documentID,
		Path:        path,
		UpdateID:    updateID,
		StateVector: base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc)),
		UpdatedAt:   time.Now().UTC(),
	}
	payload, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.metadataPath(documentID), payload, 0o644)
}

func (c *documentCache) loadLocked(meta *document) (*crdt.Doc, documentCacheMetadata, error) {
	metadata := documentCacheMetadata{
		DocumentID:  meta.ID,
		Path:        meta.Path,
		UpdateID:    meta.UpdateID,
		StateVector: meta.StateVector,
	}
	if payload, err := os.ReadFile(c.metadataPath(meta.ID)); err == nil {
		_ = json.Unmarshal(payload, &metadata)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, metadata, err
	}

	doc := crdt.New()
	state, err := os.ReadFile(c.statePath(meta.ID))
	if errors.Is(err, os.ErrNotExist) {
		if meta.CRDTState != "" {
			decoded, err := base64.StdEncoding.DecodeString(meta.CRDTState)
			if err != nil {
				return nil, metadata, err
			}
			state = decoded
		}
	} else if err != nil {
		return nil, metadata, err
	}
	if len(state) > 0 {
		if err := crdt.ApplyUpdateV1(doc, state, "cache-load"); err != nil {
			return nil, metadata, err
		}
	}
	return doc, metadata, nil
}

func (c *documentCache) fetchMissingUpdate(ctx context.Context, documentID, stateVector string) (*documentSyncResponse, error) {
	payload, err := json.Marshal(documentSyncRequest{StateVector: stateVector})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.backendURL+"/api/documents/"+url.PathEscape(documentID)+"/sync", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("document sync failed: %s", res.Status)
	}
	var response documentSyncResponse
	if err := json.NewDecoder(res.Body).Decode(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func (c *documentCache) entryFor(documentID string) *documentCacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[documentID]
	if entry == nil {
		entry = &documentCacheEntry{}
		c.entries[documentID] = entry
	}
	return entry
}

func (c *documentCache) documentDir(documentID string) string {
	return filepath.Join(c.root, safeDocumentCacheName(documentID))
}

func (c *documentCache) statePath(documentID string) string {
	return filepath.Join(c.documentDir(documentID), "state.bin")
}

func (c *documentCache) metadataPath(documentID string) string {
	return filepath.Join(c.documentDir(documentID), "metadata.json")
}

func safeDocumentCacheName(value string) string {
	var builder strings.Builder
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' {
			builder.WriteRune(char)
			continue
		}
		builder.WriteByte('_')
	}
	if builder.Len() == 0 {
		return "document"
	}
	return builder.String()
}
