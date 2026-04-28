package syncer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/reearth/ygo/crdt"
)

type documentCache struct {
	root string

	mu      sync.Mutex
	entries map[string]*documentCacheEntry
}

type documentCacheEntry struct {
	mu                  sync.Mutex
	metadata            documentCacheMetadata
	loaded              bool
	contentKnown        bool
	statePersisted      bool
	updatesSincePersist int
	lastPersistedAt     time.Time
}

type documentCacheMetadata struct {
	DocumentID  string    `json:"documentId"`
	Path        string    `json:"path"`
	UpdateID    int64     `json:"updateId,omitempty"`
	StateVector string    `json:"stateVector,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type materializedCachedDocument struct {
	Doc          *crdt.Doc
	DocMu        *sync.Mutex
	Entry        *documentCacheEntry
	Content      string
	ContentKnown bool
	UpdateID     int64
}

const documentCachePersistEveryUpdates = 100

func newDocumentCache(root string) (*documentCache, error) {
	if strings.TrimSpace(root) == "" {
		return nil, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &documentCache{
		root:    root,
		entries: map[string]*documentCacheEntry{},
	}, nil
}

func (c *documentCache) materialize(ctx context.Context, meta *document) (*materializedCachedDocument, error) {
	if meta == nil {
		return nil, errors.New("document metadata is required")
	}
	if c == nil {
		doc := crdt.New()
		return &materializedCachedDocument{
			Doc:          doc,
			DocMu:        &sync.Mutex{},
			ContentKnown: false,
			UpdateID:     meta.UpdateID,
		}, nil
	}

	entry := c.entryFor(meta.ID)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	doc, metadata, contentKnown, statePersisted, err := c.loadLocked(meta)
	if err != nil {
		return nil, err
	}
	entry.metadata = metadata
	entry.loaded = true
	entry.contentKnown = contentKnown
	entry.statePersisted = statePersisted
	if !contentKnown {
		return &materializedCachedDocument{
			Doc:          doc,
			DocMu:        &sync.Mutex{},
			Entry:        entry,
			ContentKnown: false,
			UpdateID:     entry.metadata.UpdateID,
		}, nil
	}

	entry.metadata.DocumentID = meta.ID
	if entry.metadata.Path == "" {
		entry.metadata.Path = meta.Path
	}
	if entry.metadata.UpdateID == 0 {
		entry.metadata.UpdateID = meta.UpdateID
	}
	entry.metadata.StateVector = base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc))
	entry.metadata.UpdatedAt = time.Now().UTC()
	return &materializedCachedDocument{
		Doc:          doc,
		DocMu:        &sync.Mutex{},
		Entry:        entry,
		Content:      doc.GetText("content").ToString(),
		ContentKnown: true,
		UpdateID:     entry.metadata.UpdateID,
	}, nil
}

func (c *documentCache) storeDoc(documentID, path string, updateID int64, doc *crdt.Doc) error {
	if c == nil || documentID == "" || doc == nil {
		return nil
	}
	entry := c.entryFor(documentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	stateVector := base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc))
	metadata := documentCacheMetadata{
		DocumentID:  documentID,
		Path:        path,
		UpdateID:    updateID,
		StateVector: stateVector,
		UpdatedAt:   time.Now().UTC(),
	}
	return c.storeDocLocked(entry, metadata, doc)
}

func (c *documentCache) maybeStoreDoc(documentID, path string, updateID int64, doc *crdt.Doc) error {
	if c == nil || documentID == "" || doc == nil {
		return nil
	}
	stateVector := base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(doc))
	entry := c.entryFor(documentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.loaded && entry.metadata.StateVector == stateVector {
		return nil
	}
	entry.metadata = documentCacheMetadata{
		DocumentID:  documentID,
		Path:        path,
		UpdateID:    updateID,
		StateVector: stateVector,
		UpdatedAt:   time.Now().UTC(),
	}
	entry.loaded = true
	entry.contentKnown = true
	entry.updatesSincePersist++
	if entry.statePersisted && entry.updatesSincePersist < documentCachePersistEveryUpdates {
		return nil
	}
	return c.storeDocLocked(entry, entry.metadata, doc)
}

func (c *documentCache) storeDocLocked(entry *documentCacheEntry, metadata documentCacheMetadata, doc *crdt.Doc) error {
	if err := os.MkdirAll(c.documentDir(metadata.DocumentID), 0o755); err != nil {
		return err
	}
	state := doc.EncodeStateAsUpdate()
	if err := os.WriteFile(c.statePath(metadata.DocumentID), state, 0o644); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.metadataPath(metadata.DocumentID), payload, 0o644); err != nil {
		return err
	}
	entry.metadata = metadata
	entry.loaded = true
	entry.contentKnown = true
	entry.statePersisted = true
	entry.updatesSincePersist = 0
	entry.lastPersistedAt = metadata.UpdatedAt
	return nil
}

func (c *documentCache) loadLocked(meta *document) (*crdt.Doc, documentCacheMetadata, bool, bool, error) {
	metadata := documentCacheMetadata{
		DocumentID:  meta.ID,
		Path:        meta.Path,
		UpdateID:    meta.UpdateID,
		StateVector: meta.StateVector,
	}
	if payload, err := os.ReadFile(c.metadataPath(meta.ID)); err == nil {
		_ = json.Unmarshal(payload, &metadata)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, metadata, false, false, err
	}

	doc := crdt.New()
	contentKnown := false
	statePersisted := false
	state, err := os.ReadFile(c.statePath(meta.ID))
	if errors.Is(err, os.ErrNotExist) {
		return doc, metadata, false, false, nil
	}
	if err != nil {
		return nil, metadata, false, false, err
	} else {
		contentKnown = true
		statePersisted = true
	}
	if len(state) > 0 {
		if err := crdt.ApplyUpdateV1(doc, state, "cache-load"); err != nil {
			return nil, metadata, false, false, err
		}
	}
	return doc, metadata, contentKnown, statePersisted, nil
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
