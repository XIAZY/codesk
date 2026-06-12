package syncer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "github.com/mattn/go-sqlite3"
	crdt "notty/internal/ycrdt"
)

type workspaceStore struct {
	path string
	db   *sql.DB

	mu      sync.Mutex
	entries map[string]*documentCacheEntry
}

type documentCache = workspaceStore

var documentSnapshotEveryUpdates int64 = 100

var documentReplayHook func(documentID string, fromSeq, toSeq int64, rows int)

var documentSnapshotStoreHook func(documentID string, seq int64) error

var documentSnapshotDecodeHook func(documentID string, seq int64)

var documentSnapshotHashValidationHook func(documentID string, seq int64)

var documentProjectedBaseLoadHook func(documentID string)

type documentCacheEntry struct {
	mu         sync.Mutex
	documentID string
	metadata   documentCacheMetadata
}

type documentCacheMetadata struct {
	DocumentID   string
	Path         string
	AppliedSeq   int64
	ProjectedSeq int64
	UpdatedAt    time.Time
}

type outboxUpdateRecord struct {
	ID                    string    `json:"id,omitempty"`
	Update                []byte    `json:"update"`
	UpdateSHA256          string    `json:"updateSha256"`
	ObservedProjectedSeq  int64     `json:"observedProjectedSeq,omitempty"`
	ObservedContentSHA256 string    `json:"observedContentSha256,omitempty"`
	ObservedContent       string    `json:"observedContent"`
	ObservedState         []byte    `json:"observedState"`
	SourcePath            string    `json:"sourcePath"`
	ActorID               string    `json:"actorId"`
	ActorType             string    `json:"actorType"`
	CreatedAt             time.Time `json:"createdAt"`
}

type materializedCachedDocument struct {
	Doc          *crdt.Doc
	DocMu        *sync.Mutex
	Entry        *documentCacheEntry
	ContentKnown bool
	UpdateID     int64
}

type documentRow struct {
	DocumentID          string
	Path                string
	AppliedSeq          int64
	ProjectedSeq        int64
	ProjectedTextSHA256 string
	ProjectedTextLen    int
	ProjectionKnown     bool
	UpdatedAt           time.Time
}

type documentSnapshotRow struct {
	DocumentID    string
	Seq           int64
	StateUpdate   []byte
	ContentText   string
	ContentSHA256 string
	StateSHA256   string
	CreatedAt     time.Time
}

func newDocumentCache(path string) (*documentCache, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		path = filepath.Join(path, "sync.db")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := sqliteFileDSN(path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &workspaceStore{
		path:    path,
		db:      db,
		entries: map[string]*documentCacheEntry{},
	}
	if err := store.initSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func sqliteFileDSN(path string) string {
	clean := filepath.Clean(path)
	if abs, err := filepath.Abs(clean); err == nil {
		clean = abs
	}
	dsn := url.URL{Scheme: "file", Path: filepath.ToSlash(clean)}
	query := dsn.Query()
	query.Set("_busy_timeout", "5000")
	query.Set("_journal_mode", "WAL")
	query.Set("_foreign_keys", "ON")
	dsn.RawQuery = query.Encode()
	return dsn.String()
}

func (c *workspaceStore) initSchema() error {
	if c == nil || c.db == nil {
		return nil
	}
	stmts := []string{
		`pragma journal_mode = wal`,
		`pragma busy_timeout = 5000`,
		`create table if not exists documents (
			document_id text primary key,
			path text not null,
			applied_seq integer not null default 0,
			projected_seq integer not null default 0,
			projected_text_sha256 text,
			projected_text_len integer,
			projection_known integer not null default 0,
			updated_at integer not null
		)`,
		`create table if not exists crdt_updates (
			seq integer primary key autoincrement,
			document_id text not null,
			update_sha256 text not null,
			update_bytes blob not null,
			source text not null check (source in ('base', 'local', 'remote')),
			actor_id text,
			actor_type text,
			created_at integer not null,
			unique (document_id, update_sha256)
		)`,
		`create index if not exists crdt_updates_by_doc_seq on crdt_updates(document_id, seq)`,
		`create table if not exists document_snapshots (
			document_id text not null,
			seq integer not null,
			state_update blob not null,
			content_text text not null,
			content_sha256 text not null,
			state_sha256 text not null,
			created_at integer not null,
			primary key (document_id, seq)
		)`,
		`create index if not exists document_snapshots_by_doc_seq on document_snapshots(document_id, seq desc)`,
		`create table if not exists incoming_updates (
			incoming_seq integer primary key autoincrement,
			document_id text not null,
			update_sha256 text not null,
			update_bytes blob not null,
			received_at integer not null,
			unique (document_id, update_sha256)
		)`,
		`create index if not exists incoming_updates_by_doc_seq on incoming_updates(document_id, incoming_seq)`,
		`create table if not exists content_outbox (
			id text primary key,
			document_id text not null,
			update_sha256 text not null,
			update_bytes blob not null,
			observed_projected_seq integer not null,
			observed_content_sha256 text not null,
			observed_content text,
			source_path text not null,
			actor_id text,
			actor_type text,
			attempts integer not null default 0,
			next_attempt_at integer,
			last_attempt_at integer,
			last_error text,
			created_at integer not null,
			updated_at integer not null
		)`,
		`create index if not exists content_outbox_by_doc_due on content_outbox(document_id, next_attempt_at, created_at)`,
		`create table if not exists thread_outbox (
			intent_id text primary key,
			document_id text not null,
			status text not null check (status in ('pending', 'ready', 'failed')),
			idempotency_key text not null unique,
			request_json text not null,
			resolved_payload_json text,
			actor_id text,
			actor_type text,
			run_id text,
			attempts integer not null default 0,
			next_attempt_at integer,
			last_attempt_at integer,
			last_status_code integer,
			last_error text,
			created_at integer not null,
			updated_at integer not null
		)`,
		`create index if not exists thread_outbox_materialize on thread_outbox(document_id, status, created_at)`,
		`create index if not exists thread_outbox_due on thread_outbox(status, next_attempt_at, created_at)`,
		`create table if not exists root_projection_entries (
			root_document_id text not null,
			entry_id text not null,
			kind text not null,
			content_document_id text not null,
			desired_path text not null,
			materialized_path text not null,
			active integer not null,
			projected_seq integer not null,
			updated_at integer not null,
			primary key (root_document_id, entry_id)
		)`,
		`create index if not exists root_projection_entries_by_content on root_projection_entries(root_document_id, content_document_id)`,
	}
	for _, stmt := range stmts {
		if _, err := c.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (c *workspaceStore) materialize(ctx context.Context, meta *document) (*materializedCachedDocument, error) {
	if meta == nil {
		return nil, errors.New("document metadata is required")
	}
	if c == nil {
		doc := crdt.New()
		return &materializedCachedDocument{Doc: doc, DocMu: &sync.Mutex{}, ContentKnown: false, UpdateID: meta.UpdateID}, nil
	}
	entry := c.entryFor(meta.ID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	doc, metadata, state, err := c.loadBaseDocLocked(entry, meta.ID, meta.Path)
	if err != nil {
		return nil, err
	}
	entry.metadata = metadata
	return &materializedCachedDocument{
		Doc:          doc,
		DocMu:        &sync.Mutex{},
		Entry:        entry,
		ContentKnown: len(state) > 0,
		UpdateID:     meta.UpdateID,
	}, nil
}

func (c *workspaceStore) storeDoc(documentID, path string, _ int64, doc *crdt.Doc) error {
	if c == nil || documentID == "" || doc == nil {
		return nil
	}
	entry := c.entryFor(documentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	metadata := documentCacheMetadata{DocumentID: documentID, Path: path, UpdatedAt: time.Now().UTC()}
	return c.storeDocLocked(entry, metadata, doc)
}

func (c *workspaceStore) storeDocLocked(entry *documentCacheEntry, metadata documentCacheMetadata, doc *crdt.Doc) error {
	if c == nil || doc == nil || metadata.DocumentID == "" {
		return nil
	}
	state := doc.EncodeStateAsUpdate()
	now := time.Now().UTC()
	var appliedSeq int64
	if err := c.withTx(func(tx *sql.Tx) error {
		if _, err := c.ensureDocumentTx(tx, metadata.DocumentID, metadata.Path, now); err != nil {
			return err
		}
		seq, _, err := c.insertCRDTUpdateTx(tx, metadata.DocumentID, state, "base", "", "", now)
		if err != nil {
			return err
		}
		if seq > 0 {
			if _, err := tx.Exec(`update documents
				set applied_seq = max(applied_seq, ?),
					updated_at = ?
				where document_id = ?`, seq, unixNano(now), metadata.DocumentID); err != nil {
				return err
			}
			metadata.AppliedSeq = seq
			appliedSeq = seq
		}
		metadata.UpdatedAt = now
		if entry != nil {
			entry.metadata = metadata
		}
		return nil
	}); err != nil {
		return err
	}
	if appliedSeq > 0 {
		return c.maybeCreateDocumentSnapshot(metadata.DocumentID, appliedSeq, doc)
	}
	return nil
}

func (c *workspaceStore) localStateVector(documentID string) []byte {
	if c == nil || documentID == "" {
		return nil
	}
	doc, _, state, err := c.loadBaseDoc(documentID, "")
	if err != nil {
		return nil
	}
	defer doc.Close()
	if len(state) == 0 {
		return nil
	}
	return crdt.EncodeStateVectorV1(doc)
}

func (c *workspaceStore) localStateDiff(documentID, path string, remoteStateVector []byte) ([]byte, error) {
	if c == nil || documentID == "" {
		return nil, nil
	}
	doc, _, state, err := c.loadBaseDoc(documentID, path)
	if err != nil {
		return nil, err
	}
	defer doc.Close()
	if len(state) == 0 {
		return nil, nil
	}
	return doc.EncodeStateAsUpdateV1(remoteStateVector)
}

func (c *workspaceStore) appendPendingRemoteUpdate(documentID, path string, update []byte) (bool, error) {
	if c == nil || documentID == "" || len(update) == 0 {
		return false, nil
	}
	validationDoc := crdt.New()
	if err := crdt.ApplyUpdateV1(validationDoc, update, "remote-validate"); err != nil {
		validationDoc.Close()
		return false, err
	}
	validationDoc.Close()
	entry := c.entryFor(documentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	now := time.Now().UTC()
	inserted := false
	err := c.withTx(func(tx *sql.Tx) error {
		if _, err := c.ensureDocumentTx(tx, documentID, path, now); err != nil {
			return err
		}
		_, ok, err := c.insertIncomingUpdateTx(tx, documentID, update, now)
		if err != nil {
			return err
		}
		inserted = ok
		return nil
	})
	return inserted, err
}

func (c *workspaceStore) insertIncomingUpdateTx(tx *sql.Tx, documentID string, update []byte, now time.Time) (int64, bool, error) {
	if len(update) == 0 {
		return 0, false, nil
	}
	updateHash := sha256Hex(update)
	res, err := tx.Exec(`insert or ignore into incoming_updates (
			document_id, update_sha256, update_bytes, received_at
		) values (?, ?, ?, ?)`,
		documentID, updateHash, update, unixNano(now))
	if err != nil {
		return 0, false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	var seq int64
	if affected == 0 {
		if err := tx.QueryRow(`select incoming_seq from incoming_updates where document_id = ? and update_sha256 = ?`, documentID, updateHash).Scan(&seq); err != nil {
			return 0, false, err
		}
		return seq, false, nil
	}
	if err := tx.QueryRow(`select last_insert_rowid()`).Scan(&seq); err != nil {
		return 0, false, err
	}
	return seq, true, nil
}

func (c *workspaceStore) loadBaseDoc(documentID, path string) (*crdt.Doc, documentCacheMetadata, []byte, error) {
	if c == nil {
		doc := crdt.New()
		return doc, documentCacheMetadata{DocumentID: documentID, Path: path}, nil, nil
	}
	entry := c.entryFor(documentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return c.loadBaseDocLocked(entry, documentID, path)
}

func (c *workspaceStore) loadBaseDocLocked(entry *documentCacheEntry, documentID, path string) (*crdt.Doc, documentCacheMetadata, []byte, error) {
	row, err := c.ensureDocument(documentID, path)
	if err != nil {
		return nil, documentCacheMetadata{DocumentID: documentID, Path: path}, nil, err
	}
	doc, state, _, err := c.loadDocAtSeq(documentID, row.AppliedSeq)
	if err != nil {
		return nil, row.metadata(), nil, err
	}
	metadata := row.metadata()
	if entry != nil {
		entry.metadata = metadata
	}
	return doc, metadata, state, nil
}

func (c *workspaceStore) pendingRemoteUpdateCountLocked(_ *documentCacheEntry, documentID string) (int, error) {
	if c == nil || documentID == "" {
		return 0, nil
	}
	var count int
	err := c.db.QueryRow(`select count(*) from incoming_updates where document_id = ?`, documentID).Scan(&count)
	return count, err
}

func (c *workspaceStore) pendingRemoteUpdateCount(documentID string) (int, error) {
	if c == nil || documentID == "" {
		return 0, nil
	}
	entry := c.entryFor(documentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return c.pendingRemoteUpdateCountLocked(entry, documentID)
}

func (c *workspaceStore) applyPendingRemoteUpdatesLocked(_ *documentCacheEntry, documentID string, doc *crdt.Doc) (int, int64, error) {
	if c == nil || doc == nil || documentID == "" {
		return 0, 0, nil
	}
	row, err := c.ensureDocument(documentID, "")
	if err != nil {
		return 0, 0, err
	}
	rows, err := c.db.Query(`select incoming_seq, update_bytes from incoming_updates where document_id = ? order by incoming_seq`, documentID)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	type incomingUpdate struct {
		seq    int64
		update []byte
	}
	var incoming []incomingUpdate
	count := 0
	for rows.Next() {
		var incomingSeq int64
		var update []byte
		if err := rows.Scan(&incomingSeq, &update); err != nil {
			return count, row.AppliedSeq, err
		}
		if len(update) == 0 {
			continue
		}
		if err := crdt.ApplyUpdateV1(doc, update, "remote-reconcile"); err != nil {
			return count, row.AppliedSeq, err
		}
		incoming = append(incoming, incomingUpdate{seq: incomingSeq, update: update})
		count++
	}
	if err := rows.Err(); err != nil {
		return count, row.AppliedSeq, err
	}
	if count == 0 {
		return 0, row.AppliedSeq, nil
	}
	maxSeq := row.AppliedSeq
	now := time.Now().UTC()
	err = c.withTx(func(tx *sql.Tx) error {
		for _, item := range incoming {
			seq, _, err := c.insertCRDTUpdateTx(tx, documentID, item.update, "remote", "", "", now)
			if err != nil {
				return err
			}
			if seq > maxSeq {
				maxSeq = seq
			}
			if _, err := tx.Exec(`delete from incoming_updates where document_id = ? and incoming_seq = ?`, documentID, item.seq); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`update documents set applied_seq = ?, updated_at = ? where document_id = ?`, maxSeq, unixNano(now), documentID)
		return err
	})
	if err == nil {
		err = c.maybeCreateDocumentSnapshot(documentID, maxSeq, doc)
	}
	return count, maxSeq, err
}

func (c *workspaceStore) loadOutboxUpdateLocked(entry *documentCacheEntry, documentID string) (*outboxUpdateRecord, error) {
	records, err := c.loadOutboxUpdatesLocked(entry, documentID)
	if err != nil || len(records) == 0 {
		return nil, err
	}
	return &records[0], nil
}

func (c *workspaceStore) loadOutboxUpdatesLocked(_ *documentCacheEntry, documentID string) ([]outboxUpdateRecord, error) {
	if c == nil || documentID == "" {
		return nil, nil
	}
	rows, err := c.db.Query(`select id, update_sha256, update_bytes, observed_projected_seq, observed_content_sha256, coalesce(observed_content, ''), source_path, actor_id, actor_type, created_at
		from content_outbox where document_id = ? order by created_at, id`, documentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []outboxUpdateRecord{}
	for rows.Next() {
		var record outboxUpdateRecord
		var createdAt int64
		if err := rows.Scan(&record.ID, &record.UpdateSHA256, &record.Update, &record.ObservedProjectedSeq, &record.ObservedContentSHA256, &record.ObservedContent, &record.SourcePath, &record.ActorID, &record.ActorType, &createdAt); err != nil {
			return nil, err
		}
		record.CreatedAt = time.Unix(0, createdAt).UTC()
		if record.UpdateSHA256 == "" {
			record.UpdateSHA256 = sha256Hex(record.Update)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	_ = rows.Close()
	for index := range records {
		doc, state, known, err := c.loadDocAtSeq(documentID, records[index].ObservedProjectedSeq)
		if err != nil {
			return nil, err
		}
		if known {
			records[index].ObservedState = state
			if records[index].ObservedContent == "" && records[index].ObservedContentSHA256 == sha256Hex([]byte(doc.GetText("content").ToString())) {
				records[index].ObservedContent = doc.GetText("content").ToString()
			}
		}
		doc.Close()
	}
	return records, nil
}

func (c *workspaceStore) storeOutboxUpdateLocked(entry *documentCacheEntry, documentID, path string, record outboxUpdateRecord) error {
	return c.storeOutboxUpdatesLocked(entry, documentID, path, []outboxUpdateRecord{record})
}

func (c *workspaceStore) storeOutboxUpdatesLocked(entry *documentCacheEntry, documentID, path string, records []outboxUpdateRecord) error {
	if c == nil || documentID == "" {
		return nil
	}
	now := time.Now().UTC()
	return c.withTx(func(tx *sql.Tx) error {
		row, err := c.ensureDocumentTx(tx, documentID, path, now)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`delete from content_outbox where document_id = ?`, documentID); err != nil {
			return err
		}
		for _, record := range records {
			if len(record.Update) == 0 {
				continue
			}
			if record.ID == "" {
				record.ID = "outbox_" + uuid.NewString()
			}
			record.UpdateSHA256 = sha256Hex(record.Update)
			if record.CreatedAt.IsZero() {
				record.CreatedAt = now
			}
			if record.ObservedProjectedSeq == 0 {
				record.ObservedProjectedSeq = row.ProjectedSeq
			}
			if record.ObservedContentSHA256 == "" {
				record.ObservedContentSHA256 = sha256Hex([]byte(record.ObservedContent))
			}
			if _, err := tx.Exec(`insert into content_outbox (
					id, document_id, update_sha256, update_bytes, observed_projected_seq,
					observed_content_sha256, observed_content, source_path, actor_id, actor_type,
					created_at, updated_at
				) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				record.ID, documentID, record.UpdateSHA256, record.Update, record.ObservedProjectedSeq,
				record.ObservedContentSHA256, record.ObservedContent, record.SourcePath, record.ActorID, record.ActorType,
				unixNano(record.CreatedAt), unixNano(now)); err != nil {
				return err
			}
		}
		if entry != nil {
			entry.metadata = row.metadata()
		}
		return nil
	})
}

func (c *workspaceStore) applyOutboxUpdateLocked(_ *documentCacheEntry, documentID, path string, record *outboxUpdateRecord) (int64, error) {
	if c == nil || documentID == "" || record == nil || len(record.Update) == 0 {
		return 0, nil
	}
	now := time.Now().UTC()
	var appliedSeq int64
	err := c.withTx(func(tx *sql.Tx) error {
		row, err := c.ensureDocumentTx(tx, documentID, path, now)
		if err != nil {
			return err
		}
		appliedSeq = row.AppliedSeq
		seq, _, err := c.insertCRDTUpdateTx(tx, documentID, record.Update, "local", record.ActorID, record.ActorType, now)
		if err != nil {
			return err
		}
		if seq > appliedSeq {
			appliedSeq = seq
		}
		if seq > 0 {
			if _, err := tx.Exec(`update documents
				set applied_seq = max(applied_seq, ?),
					updated_at = ?
				where document_id = ?`, seq, unixNano(now), documentID); err != nil {
				return err
			}
		}
		return nil
	})
	return appliedSeq, err
}

func (c *workspaceStore) clearOutboxUpdates(documentID string) error {
	if c == nil || documentID == "" {
		return nil
	}
	_, err := c.db.Exec(`delete from content_outbox where document_id = ?`, documentID)
	return err
}

func (c *workspaceStore) clearOutboxUpdate(documentID, id string) error {
	if c == nil || documentID == "" {
		return nil
	}
	if strings.TrimSpace(id) == "" {
		return c.clearOutboxUpdates(documentID)
	}
	_, err := c.db.Exec(`delete from content_outbox where document_id = ? and id = ?`, documentID, id)
	return err
}

func (c *workspaceStore) removeDocumentLocked(entry *documentCacheEntry, documentID string) error {
	if c == nil || documentID == "" {
		return nil
	}
	err := c.withTx(func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`delete from document_snapshots where document_id = ?`,
			`delete from thread_outbox where document_id = ?`,
			`delete from content_outbox where document_id = ?`,
			`delete from incoming_updates where document_id = ?`,
			`delete from crdt_updates where document_id = ?`,
			`delete from documents where document_id = ?`,
		} {
			if _, err := tx.Exec(stmt, documentID); err != nil {
				return err
			}
		}
		return nil
	})
	if entry != nil {
		entry.metadata = documentCacheMetadata{}
	}
	return err
}

func (c *workspaceStore) lockEntry(documentID string) (*documentCacheEntry, func()) {
	if c == nil || documentID == "" {
		return nil, func() {}
	}
	entry := c.entryFor(documentID)
	entry.mu.Lock()
	return entry, entry.mu.Unlock
}

func (c *workspaceStore) storeProjectedBase(documentID, content string, state []byte, projectedSeq int64) error {
	if c == nil || documentID == "" {
		return nil
	}
	now := time.Now().UTC()
	return c.withTx(func(tx *sql.Tx) error {
		if _, err := c.ensureDocumentTx(tx, documentID, "", now); err != nil {
			return err
		}
		if _, err := tx.Exec(`update documents
			set projected_seq = ?,
				projected_text_sha256 = ?,
				projected_text_len = ?,
				projection_known = 1,
				updated_at = ?
			where document_id = ?`, projectedSeq, sha256Hex([]byte(content)), len(content), unixNano(now), documentID); err != nil {
			return err
		}
		return nil
	})
}

func (c *workspaceStore) documentAppliedSeq(documentID string) (int64, error) {
	if c == nil || documentID == "" {
		return 0, nil
	}
	row, err := c.ensureDocument(documentID, "")
	if err != nil {
		return 0, err
	}
	return row.AppliedSeq, nil
}

func (c *workspaceStore) storeProjectedSeq(documentID string, projectedSeq int64) error {
	if c == nil || documentID == "" {
		return nil
	}
	now := time.Now().UTC()
	_, err := c.db.Exec(`update documents
		set projected_seq = ?,
			projection_known = 1,
			updated_at = ?
		where document_id = ?`, projectedSeq, unixNano(now), documentID)
	return err
}

func (c *workspaceStore) loadRootProjectionEntries(rootDocumentID string) (map[string]rootProjectionEntry, error) {
	result := map[string]rootProjectionEntry{}
	if c == nil || rootDocumentID == "" {
		return result, nil
	}
	rows, err := c.db.Query(`select entry_id, kind, content_document_id, desired_path, materialized_path, active, projected_seq
		from root_projection_entries where root_document_id = ?`, rootDocumentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var entry rootProjectionEntry
		var active int
		if err := rows.Scan(&entry.EntryID, &entry.Kind, &entry.ContentDocumentID, &entry.DesiredPath, &entry.MaterializedPath, &active, &entry.ProjectedSeq); err != nil {
			return nil, err
		}
		entry.Active = active != 0
		result[entry.EntryID] = entry
	}
	return result, rows.Err()
}

func (c *workspaceStore) storeRootProjectionEntries(rootDocumentID string, entries []rootProjectionEntry) error {
	if c == nil || rootDocumentID == "" {
		return nil
	}
	now := time.Now().UTC()
	return c.withTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`delete from root_projection_entries where root_document_id = ?`, rootDocumentID); err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.EntryID == "" || entry.ContentDocumentID == "" {
				continue
			}
			active := 0
			if entry.Active {
				active = 1
			}
			if _, err := tx.Exec(`insert into root_projection_entries (
					root_document_id, entry_id, kind, content_document_id, desired_path,
					materialized_path, active, projected_seq, updated_at
				) values (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				rootDocumentID, entry.EntryID, firstNonEmptyText(entry.Kind, rootEntryKindFile),
				entry.ContentDocumentID, entry.DesiredPath, entry.MaterializedPath, active,
				entry.ProjectedSeq, unixNano(now)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (c *workspaceStore) loadProjectedBase(documentID string) (string, []byte, bool, error) {
	if c == nil || documentID == "" {
		return "", nil, false, nil
	}
	if documentProjectedBaseLoadHook != nil {
		documentProjectedBaseLoadHook(documentID)
	}
	row, err := c.ensureDocument(documentID, "")
	if err != nil {
		return "", nil, false, err
	}
	if !row.ProjectionKnown {
		return "", nil, false, nil
	}
	if content, state, known, err := c.loadProjectedBaseSnapshot(row); err != nil || known {
		return content, state, known, err
	}
	doc, state, known, err := c.loadDocAtSeq(documentID, row.ProjectedSeq)
	if err != nil {
		return "", nil, false, err
	}
	defer doc.Close()
	if !known {
		return "", nil, false, nil
	}
	content := doc.GetText("content").ToString()
	if row.ProjectedTextSHA256 != "" && row.ProjectedTextSHA256 != sha256Hex([]byte(content)) {
		return "", nil, false, nil
	}
	return content, state, true, nil
}

func (c *workspaceStore) documentContentKnown(documentID string) (bool, error) {
	row, err := c.ensureDocument(documentID, "")
	if err != nil {
		return false, err
	}
	return row.AppliedSeq > 0, nil
}

func (c *workspaceStore) documentsNeedingReconcile() ([]string, bool, error) {
	if c == nil {
		return nil, false, nil
	}
	rows, err := c.db.Query(`
		select document_id from documents
		where exists (select 1 from content_outbox where content_outbox.document_id = documents.document_id)
		   or exists (select 1 from thread_outbox where thread_outbox.document_id = documents.document_id and status = 'pending')
		   or exists (select 1 from incoming_updates where incoming_updates.document_id = documents.document_id)
		order by document_id`)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, false, err
		}
		ids = append(ids, id)
	}
	var readyCount int
	if err := c.db.QueryRow(`select count(*) from thread_outbox where status = 'ready'`).Scan(&readyCount); err != nil {
		return nil, false, err
	}
	return ids, readyCount > 0, rows.Err()
}

func (c *workspaceStore) withTx(fn func(*sql.Tx) error) error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (c *workspaceStore) ensureDocument(documentID, path string) (documentRow, error) {
	if c == nil || documentID == "" {
		return documentRow{DocumentID: documentID, Path: path}, nil
	}
	var row documentRow
	now := time.Now().UTC()
	err := c.withTx(func(tx *sql.Tx) error {
		next, err := c.ensureDocumentTx(tx, documentID, path, now)
		if err != nil {
			return err
		}
		row = next
		return nil
	})
	return row, err
}

func (c *workspaceStore) ensureDocumentTx(tx *sql.Tx, documentID, path string, now time.Time) (documentRow, error) {
	if documentID == "" {
		return documentRow{}, errors.New("document id is required")
	}
	if _, err := tx.Exec(`insert into documents (document_id, path, updated_at)
		values (?, ?, ?)
		on conflict(document_id) do update set
			path = case when excluded.path != '' then excluded.path else documents.path end,
			updated_at = excluded.updated_at`,
		documentID, path, unixNano(now)); err != nil {
		return documentRow{}, err
	}
	return c.loadDocumentRowTx(tx, documentID)
}

func (c *workspaceStore) loadDocumentRowTx(tx *sql.Tx, documentID string) (documentRow, error) {
	var row documentRow
	var projectedHash sql.NullString
	var projectedLen sql.NullInt64
	var updatedAt int64
	var projectionKnown int
	err := tx.QueryRow(`select document_id, path, applied_seq, projected_seq, projected_text_sha256, projected_text_len, projection_known, updated_at
		from documents where document_id = ?`, documentID).Scan(
		&row.DocumentID, &row.Path, &row.AppliedSeq, &row.ProjectedSeq,
		&projectedHash, &projectedLen, &projectionKnown, &updatedAt,
	)
	if err != nil {
		return row, err
	}
	row.ProjectedTextSHA256 = projectedHash.String
	if projectedLen.Valid {
		row.ProjectedTextLen = int(projectedLen.Int64)
	}
	row.ProjectionKnown = projectionKnown != 0
	row.UpdatedAt = time.Unix(0, updatedAt).UTC()
	return row, nil
}

func (c *workspaceStore) insertCRDTUpdateTx(tx *sql.Tx, documentID string, update []byte, source string, actorID, actorType string, now time.Time) (int64, bool, error) {
	if len(update) == 0 {
		return 0, false, nil
	}
	updateHash := sha256Hex(update)
	res, err := tx.Exec(`insert or ignore into crdt_updates (
			document_id, update_sha256, update_bytes, source, actor_id, actor_type, created_at
		) values (?, ?, ?, ?, ?, ?, ?)`,
		documentID, updateHash, update, source, actorID, actorType, unixNano(now))
	if err != nil {
		return 0, false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	var seq int64
	if affected == 0 {
		if err := tx.QueryRow(`select seq from crdt_updates where document_id = ? and update_sha256 = ?`, documentID, updateHash).Scan(&seq); err != nil {
			return 0, false, err
		}
		return seq, false, nil
	}
	if err := tx.QueryRow(`select last_insert_rowid()`).Scan(&seq); err != nil {
		return 0, false, err
	}
	return seq, true, nil
}

func (c *workspaceStore) storeDocumentSnapshot(documentID string, seq int64, state []byte, content string) error {
	if c == nil || documentID == "" || seq <= 0 || len(state) == 0 {
		return nil
	}
	now := time.Now().UTC()
	return c.withTx(func(tx *sql.Tx) error {
		return c.storeDocumentSnapshotTx(tx, documentID, seq, state, content, now)
	})
}

func (c *workspaceStore) storeDocumentSnapshotTx(tx *sql.Tx, documentID string, seq int64, state []byte, content string, now time.Time) error {
	if tx == nil || documentID == "" || seq <= 0 || len(state) == 0 {
		return nil
	}
	if documentSnapshotStoreHook != nil {
		if err := documentSnapshotStoreHook(documentID, seq); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`insert into document_snapshots (
			document_id, seq, state_update, content_text, content_sha256, state_sha256, created_at
		) values (?, ?, ?, ?, ?, ?, ?)
		on conflict(document_id, seq) do update set
			state_update = excluded.state_update,
			content_text = excluded.content_text,
			content_sha256 = excluded.content_sha256,
			state_sha256 = excluded.state_sha256,
			created_at = excluded.created_at`,
		documentID, seq, state, content, sha256Hex([]byte(content)), sha256Hex(state), unixNano(now))
	return err
}

func (c *workspaceStore) maybeCreateDocumentSnapshot(documentID string, seq int64, doc *crdt.Doc) error {
	if c == nil || documentID == "" || seq <= 0 || doc == nil {
		return nil
	}
	latestSeq, ok, err := c.latestDocumentSnapshotSeq(documentID)
	if err != nil {
		return nil
	}
	if ok && seq-latestSeq < documentSnapshotEveryUpdates {
		return nil
	}
	state := doc.EncodeStateAsUpdate()
	if len(state) == 0 {
		return nil
	}
	_ = c.storeDocumentSnapshot(documentID, seq, state, doc.GetText("content").ToString())
	return nil
}

func (c *workspaceStore) latestDocumentSnapshotSeq(documentID string) (int64, bool, error) {
	if c == nil || documentID == "" {
		return 0, false, nil
	}
	var seq int64
	err := c.db.QueryRow(`select seq from document_snapshots where document_id = ? order by seq desc limit 1`, documentID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return seq, true, nil
}

func (c *workspaceStore) loadDocumentSnapshotAt(documentID string, seq int64) (documentSnapshotRow, bool, error) {
	if c == nil || documentID == "" || seq <= 0 {
		return documentSnapshotRow{}, false, nil
	}
	return c.loadDocumentSnapshotByQuery(`select document_id, seq, state_update, content_text, content_sha256, state_sha256, created_at
		from document_snapshots where document_id = ? and seq = ?`, documentID, seq)
}

func (c *workspaceStore) loadLatestSnapshotBeforeOrAt(documentID string, seq int64) (documentSnapshotRow, bool, error) {
	if c == nil || documentID == "" || seq <= 0 {
		return documentSnapshotRow{}, false, nil
	}
	upperSeq := seq
	for upperSeq > 0 {
		row, ok, err := c.loadDocumentSnapshotByQuery(`select document_id, seq, state_update, content_text, content_sha256, state_sha256, created_at
			from document_snapshots where document_id = ? and seq <= ? order by seq desc limit 1`, documentID, upperSeq)
		if err != nil || !ok {
			return row, ok, err
		}
		if row.metadataValid() {
			return row, true, nil
		}
		_ = c.deleteDocumentSnapshot(row.DocumentID, row.Seq)
		upperSeq = row.Seq - 1
	}
	return documentSnapshotRow{}, false, nil
}

func (c *workspaceStore) loadDocumentSnapshotByQuery(query string, args ...any) (documentSnapshotRow, bool, error) {
	var row documentSnapshotRow
	var createdAt int64
	err := c.db.QueryRow(query, args...).Scan(
		&row.DocumentID, &row.Seq, &row.StateUpdate, &row.ContentText,
		&row.ContentSHA256, &row.StateSHA256, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return row, false, nil
	}
	if err != nil {
		return row, false, err
	}
	row.CreatedAt = time.Unix(0, createdAt).UTC()
	return row, true, nil
}

func (c *workspaceStore) loadProjectedBaseSnapshot(row documentRow) (string, []byte, bool, error) {
	if c == nil || row.DocumentID == "" || row.ProjectedSeq <= 0 {
		return "", nil, false, nil
	}
	snapshot, ok, err := c.loadDocumentSnapshotAt(row.DocumentID, row.ProjectedSeq)
	if err != nil || !ok {
		return "", nil, false, nil
	}
	if !snapshot.metadataValid() ||
		(row.ProjectedTextSHA256 != "" && row.ProjectedTextSHA256 != snapshot.ContentSHA256) ||
		(row.ProjectedTextLen != 0 && row.ProjectedTextLen != len(snapshot.ContentText)) {
		_ = c.deleteDocumentSnapshot(snapshot.DocumentID, snapshot.Seq)
		return "", nil, false, nil
	}
	return snapshot.ContentText, append([]byte(nil), snapshot.StateUpdate...), true, nil
}

func (c *workspaceStore) deleteDocumentSnapshot(documentID string, seq int64) error {
	if c == nil || documentID == "" || seq <= 0 {
		return nil
	}
	_, err := c.db.Exec(`delete from document_snapshots where document_id = ? and seq = ?`, documentID, seq)
	return err
}

func (row documentSnapshotRow) hashesValid() bool {
	if documentSnapshotHashValidationHook != nil {
		documentSnapshotHashValidationHook(row.DocumentID, row.Seq)
	}
	return row.DocumentID != "" &&
		row.Seq > 0 &&
		len(row.StateUpdate) > 0 &&
		row.ContentSHA256 == sha256Hex([]byte(row.ContentText)) &&
		row.StateSHA256 == sha256Hex(row.StateUpdate)
}

func (row documentSnapshotRow) metadataValid() bool {
	return row.DocumentID != "" &&
		row.Seq > 0 &&
		len(row.StateUpdate) > 0 &&
		row.ContentSHA256 != "" &&
		row.StateSHA256 != ""
}

func (c *workspaceStore) docFromSnapshot(row documentSnapshotRow) (*crdt.Doc, bool, error) {
	doc := crdt.New()
	if !row.metadataValid() {
		return doc, false, nil
	}
	if documentSnapshotDecodeHook != nil {
		documentSnapshotDecodeHook(row.DocumentID, row.Seq)
	}
	if err := crdt.ApplyUpdateV1(doc, row.StateUpdate, "sqlite-snapshot"); err != nil {
		doc.Close()
		return nil, false, nil
	}
	return doc, true, nil
}

func (c *workspaceStore) replayCRDTUpdates(doc *crdt.Doc, documentID string, fromExclusiveSeq, targetSeq int64) (bool, int, error) {
	if c == nil || doc == nil || documentID == "" || targetSeq <= fromExclusiveSeq {
		return false, 0, nil
	}
	rows, err := c.db.Query(`select update_bytes from crdt_updates where document_id = ? and seq > ? and seq <= ? order by seq`, documentID, fromExclusiveSeq, targetSeq)
	if err != nil {
		return false, 0, err
	}
	defer rows.Close()
	known := false
	count := 0
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			return known, count, err
		}
		if len(update) == 0 {
			continue
		}
		if err := crdt.ApplyUpdateV1(doc, update, "sqlite-load"); err != nil {
			return known, count, err
		}
		known = true
		count++
	}
	if err := rows.Err(); err != nil {
		return known, count, err
	}
	if documentReplayHook != nil {
		documentReplayHook(documentID, fromExclusiveSeq, targetSeq, count)
	}
	return known, count, nil
}

func (c *workspaceStore) loadDocAtSeq(documentID string, seq int64) (*crdt.Doc, []byte, bool, error) {
	if c == nil || documentID == "" || seq <= 0 {
		doc := crdt.New()
		return doc, nil, false, nil
	}
	snapshot, snapshotKnown, err := c.loadLatestSnapshotBeforeOrAt(documentID, seq)
	if err != nil {
		return nil, nil, false, err
	}
	doc := crdt.New()
	startSeq := int64(0)
	known := false
	if snapshotKnown {
		doc.Close()
		doc, known, err = c.docFromSnapshot(snapshot)
		if err != nil {
			return nil, nil, false, err
		}
		if !known {
			if doc != nil {
				doc.Close()
			}
			if err := c.deleteDocumentSnapshot(snapshot.DocumentID, snapshot.Seq); err != nil {
				return nil, nil, false, err
			}
			return c.loadDocAtSeq(documentID, seq)
		}
		startSeq = snapshot.Seq
		if startSeq == seq {
			return doc, append([]byte(nil), snapshot.StateUpdate...), true, nil
		}
	}
	tailKnown, _, err := c.replayCRDTUpdates(doc, documentID, startSeq, seq)
	if err != nil {
		doc.Close()
		return nil, nil, false, err
	}
	known = known || tailKnown
	if !known {
		return doc, nil, false, nil
	}
	state := doc.EncodeStateAsUpdate()
	if snapshotKnown && startSeq < seq || !snapshotKnown {
		_ = c.maybeCreateDocumentSnapshot(documentID, seq, doc)
	}
	return doc, state, true, nil
}

func (c *workspaceStore) entryFor(documentID string) *documentCacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[documentID]
	if entry == nil {
		entry = &documentCacheEntry{documentID: documentID}
		c.entries[documentID] = entry
	}
	return entry
}

func (r documentRow) metadata() documentCacheMetadata {
	return documentCacheMetadata{
		DocumentID:   r.DocumentID,
		Path:         r.Path,
		AppliedSeq:   r.AppliedSeq,
		ProjectedSeq: r.ProjectedSeq,
		UpdatedAt:    r.UpdatedAt,
	}
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

func unixNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UTC().UnixNano()
}

func nullableUnixNano(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return unixNano(t)
}

func timeFromNullable(ns sql.NullInt64) time.Time {
	if !ns.Valid || ns.Int64 == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns.Int64).UTC()
}

func marshalJSON(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func unmarshalJSON[T any](payload string, target *T) error {
	if strings.TrimSpace(payload) == "" {
		return nil
	}
	return json.Unmarshal([]byte(payload), target)
}

func sortThreadIntents(intents []threadOutboxIntent) {
	sort.Slice(intents, func(i, j int) bool {
		if intents[i].CreatedAt.Equal(intents[j].CreatedAt) {
			return intents[i].IntentID < intents[j].IntentID
		}
		return intents[i].CreatedAt.Before(intents[j].CreatedAt)
	})
}
