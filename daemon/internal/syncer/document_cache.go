package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	crdt "notty/internal/ycrdt"
)

var errPendingUpdateLogHashMismatch = errors.New("cached pending update log hash mismatch")
var errOutboxUpdateHashMismatch = errors.New("cached outgoing update outbox hash mismatch")

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
	pendingHash         string
	pendingHashLoaded   bool
	outboxHash          string
	outboxHashLoaded    bool
	seenUpdateHashes    map[string]struct{}
}

type documentCacheMetadata struct {
	DocumentID     string    `json:"documentId"`
	Path           string    `json:"path"`
	UpdateID       int64     `json:"updateId,omitempty"`
	StateVector    string    `json:"stateVector,omitempty"`
	StateSHA256    string    `json:"stateSha256,omitempty"`
	PendingSHA256  string    `json:"pendingSha256,omitempty"`
	PendingUpdates int       `json:"pendingUpdates,omitempty"`
	OutboxSHA256   string    `json:"outboxSha256,omitempty"`
	OutboxUpdates  int       `json:"outboxUpdates,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type outboxUpdateRecord struct {
	Update            []byte    `json:"update"`
	UpdateSHA256      string    `json:"updateSha256"`
	TargetStateVector []byte    `json:"targetStateVector"`
	ObservedContent   string    `json:"observedContent"`
	ObservedState     []byte    `json:"observedState"`
	SourcePath        string    `json:"sourcePath"`
	CreatedAt         time.Time `json:"createdAt"`
}

type materializedCachedDocument struct {
	Doc          *crdt.Doc
	DocMu        *sync.Mutex
	Entry        *documentCacheEntry
	ContentKnown bool
	UpdateID     int64
}

const documentCachePersistEveryUpdates = 100

var emptyDocumentStateVector = base64.StdEncoding.EncodeToString(crdt.EncodeStateVectorV1(crdt.New()))

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
	metadata.StateSHA256 = sha256Hex(state)
	if err := os.WriteFile(c.statePath(metadata.DocumentID), state, 0o644); err != nil {
		return err
	}
	if entry.pendingHashLoaded {
		metadata.PendingSHA256 = entry.pendingHash
	}
	if entry.outboxHashLoaded {
		metadata.OutboxSHA256 = entry.outboxHash
		metadata.OutboxUpdates = entry.metadata.OutboxUpdates
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
		if metadata.StateVector == emptyDocumentStateVector {
			return doc, metadata, true, false, nil
		}
		return doc, metadata, false, false, nil
	}
	if err != nil {
		return nil, metadata, false, false, err
	} else {
		contentKnown = true
		statePersisted = true
	}
	if len(state) > 0 {
		if metadata.StateSHA256 != "" && metadata.StateSHA256 != sha256Hex(state) {
			_ = os.Remove(c.statePath(meta.ID))
			metadata.StateSHA256 = ""
			metadata.StateVector = ""
			return doc, metadata, false, false, nil
		}
		if err := crdt.ApplyUpdateV1(doc, state, "cache-load"); err != nil {
			_ = os.Remove(c.statePath(meta.ID))
			metadata.StateSHA256 = ""
			metadata.StateVector = ""
			return crdt.New(), metadata, false, false, nil
		}
	}
	return doc, metadata, contentKnown, statePersisted, nil
}

func (c *documentCache) cachedStateVector(documentID string) []byte {
	if c == nil || documentID == "" {
		return nil
	}
	entry := c.entryFor(documentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !entry.loaded {
		if payload, err := os.ReadFile(c.metadataPath(documentID)); err == nil {
			_ = json.Unmarshal(payload, &entry.metadata)
			entry.loaded = true
		}
	}
	if entry.metadata.StateVector == "" {
		return nil
	}
	stateVector, err := base64.StdEncoding.DecodeString(entry.metadata.StateVector)
	if err != nil {
		return nil
	}
	return stateVector
}

func (c *documentCache) appendPendingRemoteUpdate(documentID, path string, update []byte) (bool, error) {
	if c == nil || documentID == "" || len(update) == 0 {
		return false, nil
	}
	entry := c.entryFor(documentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if err := c.ensurePendingHashLoadedLocked(entry, documentID); err != nil {
		if !errors.Is(err, errPendingUpdateLogHashMismatch) {
			return false, err
		}
		if clearErr := c.clearPendingRemoteUpdatesLocked(entry, documentID); clearErr != nil {
			return false, clearErr
		}
	}
	updateHash := sha256Hex(update)
	if entry.seenUpdateHashes == nil {
		entry.seenUpdateHashes = map[string]struct{}{}
	}
	if _, ok := entry.seenUpdateHashes[updateHash]; ok {
		return false, nil
	}
	if err := os.MkdirAll(c.documentDir(documentID), 0o755); err != nil {
		return false, err
	}
	frame := frameUpdate(update)
	file, err := os.OpenFile(c.pendingPath(documentID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	if _, err := file.Write(frame); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	entry.seenUpdateHashes[updateHash] = struct{}{}
	pendingHash, pendingCount, err := c.pendingLogHashAndCount(documentID)
	if err != nil {
		return false, err
	}
	entry.pendingHash = pendingHash
	entry.pendingHashLoaded = true
	entry.metadata.DocumentID = documentID
	if entry.metadata.Path == "" {
		entry.metadata.Path = path
	}
	entry.metadata.PendingSHA256 = pendingHash
	entry.metadata.PendingUpdates = pendingCount
	entry.metadata.UpdatedAt = time.Now().UTC()
	if err := c.writeMetadataLocked(entry, entry.metadata); err != nil {
		return false, err
	}
	return true, nil
}

func (c *documentCache) loadBaseDoc(documentID, path string) (*crdt.Doc, documentCacheMetadata, []byte, error) {
	if c == nil {
		doc := crdt.New()
		return doc, documentCacheMetadata{DocumentID: documentID, Path: path}, nil, nil
	}
	entry := c.entryFor(documentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return c.loadBaseDocLocked(entry, documentID, path)
}

func (c *documentCache) loadBaseDocLocked(entry *documentCacheEntry, documentID, path string) (*crdt.Doc, documentCacheMetadata, []byte, error) {
	metadata := documentCacheMetadata{
		DocumentID: documentID,
		Path:       path,
	}
	if payload, err := os.ReadFile(c.metadataPath(documentID)); err == nil {
		_ = json.Unmarshal(payload, &metadata)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, metadata, nil, err
	}
	state, err := os.ReadFile(c.statePath(documentID))
	if errors.Is(err, os.ErrNotExist) {
		state = nil
	} else if err != nil {
		return nil, metadata, nil, err
	}
	stateCleared := false
	if metadata.StateSHA256 != "" && metadata.StateSHA256 != sha256Hex(state) {
		_ = os.Remove(c.statePath(documentID))
		metadata.StateSHA256 = ""
		metadata.StateVector = ""
		state = nil
		stateCleared = true
	}
	doc := crdt.New()
	if len(state) > 0 {
		if err := crdt.ApplyUpdateV1(doc, state, "cache-load"); err != nil {
			_ = os.Remove(c.statePath(documentID))
			metadata.StateSHA256 = ""
			metadata.StateVector = ""
			state = nil
			doc = crdt.New()
			stateCleared = true
		}
	}
	entry.metadata = metadata
	entry.loaded = true
	if stateCleared {
		_ = c.writeMetadataLocked(entry, metadata)
	}
	return doc, metadata, state, nil
}

func (c *documentCache) loadPendingRemoteUpdatesLocked(entry *documentCacheEntry, documentID string) ([][]byte, error) {
	if err := c.ensurePendingHashLoadedLocked(entry, documentID); err != nil {
		return nil, err
	}
	updates, err := readFramedUpdates(c.pendingPath(documentID))
	if err != nil {
		return nil, err
	}
	if entry.pendingHashLoaded {
		actualHash, _, err := c.pendingLogHashAndCount(documentID)
		if err != nil {
			return nil, err
		}
		if entry.pendingHash != actualHash {
			return nil, errPendingUpdateLogHashMismatch
		}
	}
	return updates, nil
}

func (c *documentCache) pendingRemoteUpdateCountLocked(entry *documentCacheEntry, documentID string) (int, error) {
	if err := c.ensurePendingHashLoadedLocked(entry, documentID); err != nil {
		return 0, err
	}
	return entry.metadata.PendingUpdates, nil
}

func (c *documentCache) forEachPendingRemoteUpdateLocked(entry *documentCacheEntry, documentID string, fn func([]byte) error) error {
	if err := c.ensurePendingHashLoadedLocked(entry, documentID); err != nil {
		return err
	}
	actualHash, count, err := c.scanPendingLog(documentID, false, func(update []byte) error {
		return fn(update)
	})
	if err != nil {
		return err
	}
	if entry.pendingHashLoaded && entry.pendingHash != actualHash {
		return errPendingUpdateLogHashMismatch
	}
	entry.metadata.PendingSHA256 = actualHash
	entry.metadata.PendingUpdates = count
	return nil
}

func (c *documentCache) clearPendingRemoteUpdatesLocked(entry *documentCacheEntry, documentID string) error {
	if err := os.Remove(c.pendingPath(documentID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entry.pendingHash = sha256Hex(nil)
	entry.pendingHashLoaded = true
	entry.metadata.PendingSHA256 = entry.pendingHash
	entry.metadata.PendingUpdates = 0
	entry.metadata.UpdatedAt = time.Now().UTC()
	return c.writeMetadataLocked(entry, entry.metadata)
}

func (c *documentCache) loadOutboxUpdateLocked(entry *documentCacheEntry, documentID string) (*outboxUpdateRecord, error) {
	if err := c.ensureOutboxHashLoadedLocked(entry, documentID); err != nil {
		return nil, err
	}
	payload, err := os.ReadFile(c.outboxPath(documentID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if entry.outboxHashLoaded && entry.outboxHash != sha256Hex(payload) {
		return nil, errOutboxUpdateHashMismatch
	}
	var record outboxUpdateRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		return nil, err
	}
	if record.UpdateSHA256 == "" {
		record.UpdateSHA256 = sha256Hex(record.Update)
	}
	if record.UpdateSHA256 != sha256Hex(record.Update) {
		return nil, errOutboxUpdateHashMismatch
	}
	return &record, nil
}

func (c *documentCache) storeOutboxUpdateLocked(entry *documentCacheEntry, documentID, path string, record outboxUpdateRecord) error {
	if c == nil || documentID == "" || len(record.Update) == 0 {
		return nil
	}
	if err := os.MkdirAll(c.documentDir(documentID), 0o755); err != nil {
		return err
	}
	record.UpdateSHA256 = sha256Hex(record.Update)
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(c.outboxPath(documentID), payload, 0o644); err != nil {
		return err
	}
	entry.outboxHash = sha256Hex(payload)
	entry.outboxHashLoaded = true
	if entry.seenUpdateHashes == nil {
		entry.seenUpdateHashes = map[string]struct{}{}
	}
	entry.seenUpdateHashes[record.UpdateSHA256] = struct{}{}
	entry.metadata.DocumentID = documentID
	if entry.metadata.Path == "" {
		entry.metadata.Path = path
	}
	entry.metadata.OutboxSHA256 = entry.outboxHash
	entry.metadata.OutboxUpdates = 1
	entry.metadata.UpdatedAt = time.Now().UTC()
	return c.writeMetadataLocked(entry, entry.metadata)
}

func (c *documentCache) clearOutboxUpdateLocked(entry *documentCacheEntry, documentID string) error {
	if err := os.Remove(c.outboxPath(documentID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entry.outboxHash = sha256Hex(nil)
	entry.outboxHashLoaded = true
	entry.metadata.OutboxSHA256 = entry.outboxHash
	entry.metadata.OutboxUpdates = 0
	entry.metadata.UpdatedAt = time.Now().UTC()
	return c.writeMetadataLocked(entry, entry.metadata)
}

func (c *documentCache) lockEntry(documentID string) (*documentCacheEntry, func()) {
	if c == nil || documentID == "" {
		return nil, func() {}
	}
	entry := c.entryFor(documentID)
	entry.mu.Lock()
	return entry, entry.mu.Unlock
}

func (c *documentCache) pendingRemoteUpdateCount(documentID string) (int, error) {
	if c == nil || documentID == "" {
		return 0, nil
	}
	_, count, err := c.pendingLogHashAndCount(documentID)
	return count, err
}

func (c *documentCache) ensurePendingHashLoadedLocked(entry *documentCacheEntry, documentID string) error {
	if entry.pendingHashLoaded {
		return nil
	}
	if !entry.loaded {
		if payload, err := os.ReadFile(c.metadataPath(documentID)); err == nil {
			_ = json.Unmarshal(payload, &entry.metadata)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		entry.loaded = true
	}
	hash, count, seen, err := c.pendingLogHashCountAndSeen(documentID, entry.seenUpdateHashes == nil)
	if err != nil {
		return err
	}
	if entry.metadata.PendingSHA256 != "" && entry.metadata.PendingSHA256 != hash {
		return errPendingUpdateLogHashMismatch
	}
	entry.pendingHash = hash
	entry.pendingHashLoaded = true
	entry.metadata.PendingSHA256 = hash
	entry.metadata.PendingUpdates = count
	if seen != nil {
		entry.seenUpdateHashes = seen
	}
	return nil
}

func (c *documentCache) ensureOutboxHashLoadedLocked(entry *documentCacheEntry, documentID string) error {
	if entry.outboxHashLoaded {
		return nil
	}
	if !entry.loaded {
		if payload, err := os.ReadFile(c.metadataPath(documentID)); err == nil {
			_ = json.Unmarshal(payload, &entry.metadata)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		entry.loaded = true
	}
	payload, err := os.ReadFile(c.outboxPath(documentID))
	if errors.Is(err, os.ErrNotExist) {
		entry.outboxHash = sha256Hex(nil)
		entry.outboxHashLoaded = true
		entry.metadata.OutboxSHA256 = entry.outboxHash
		entry.metadata.OutboxUpdates = 0
		return nil
	}
	if err != nil {
		return err
	}
	hash := sha256Hex(payload)
	if entry.metadata.OutboxSHA256 != "" && entry.metadata.OutboxSHA256 != hash {
		return errOutboxUpdateHashMismatch
	}
	entry.outboxHash = hash
	entry.outboxHashLoaded = true
	entry.metadata.OutboxSHA256 = hash
	entry.metadata.OutboxUpdates = 1
	return nil
}

func (c *documentCache) pendingLogHashAndCount(documentID string) (string, int, error) {
	hash, count, _, err := c.pendingLogHashCountAndSeen(documentID, false)
	return hash, count, err
}

func (c *documentCache) pendingLogHashCountAndSeen(documentID string, collectSeen bool) (string, int, map[string]struct{}, error) {
	var seen map[string]struct{}
	hash, count, err := c.scanPendingLog(documentID, collectSeen, func(update []byte) error {
		if seen == nil && collectSeen {
			seen = map[string]struct{}{}
		}
		if seen != nil {
			seen[sha256Hex(update)] = struct{}{}
		}
		return nil
	})
	return hash, count, seen, err
}

func (c *documentCache) scanPendingLog(documentID string, collectSeen bool, fn func([]byte) error) (string, int, error) {
	file, err := os.Open(c.pendingPath(documentID))
	if errors.Is(err, os.ErrNotExist) {
		return sha256Hex(nil), 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	defer file.Close()

	hash := sha256.New()
	var header [8]byte
	count := 0
	for {
		if _, err := io.ReadFull(file, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", 0, err
		}
		if _, err := hash.Write(header[:]); err != nil {
			return "", 0, err
		}
		length := binary.BigEndian.Uint64(header[:])
		if length > uint64(int(^uint(0)>>1)) {
			return "", 0, errors.New("pending update frame too large")
		}
		update := make([]byte, int(length))
		if _, err := io.ReadFull(file, update); err != nil {
			return "", 0, err
		}
		if _, err := hash.Write(update); err != nil {
			return "", 0, err
		}
		if fn != nil {
			if err := fn(update); err != nil {
				return "", 0, err
			}
		}
		count++
	}
	return hex.EncodeToString(hash.Sum(nil)), count, nil
}

func (c *documentCache) writeMetadataLocked(entry *documentCacheEntry, metadata documentCacheMetadata) error {
	if err := os.MkdirAll(c.documentDir(metadata.DocumentID), 0o755); err != nil {
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
	return nil
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

func (c *documentCache) pendingPath(documentID string) string {
	return filepath.Join(c.documentDir(documentID), "pending_remote.log")
}

func (c *documentCache) outboxPath(documentID string) string {
	return filepath.Join(c.documentDir(documentID), "outbox_update.json")
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

func sha256Hex(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func frameUpdate(update []byte) []byte {
	frame := make([]byte, 8+len(update))
	binary.BigEndian.PutUint64(frame[:8], uint64(len(update)))
	copy(frame[8:], update)
	return frame
}

func readFramedUpdates(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeFramedUpdates(data)
}

func decodeFramedUpdates(data []byte) ([][]byte, error) {
	updates := make([][]byte, 0)
	for len(data) > 0 {
		if len(data) < 8 {
			return nil, io.ErrUnexpectedEOF
		}
		length := binary.BigEndian.Uint64(data[:8])
		data = data[8:]
		if length > uint64(len(data)) {
			return nil, io.ErrUnexpectedEOF
		}
		update := make([]byte, int(length))
		copy(update, data[:length])
		updates = append(updates, update)
		data = data[length:]
	}
	return updates, nil
}
