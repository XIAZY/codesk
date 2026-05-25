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

type documentCacheEntry struct {
	mu         sync.Mutex
	documentID string
	metadata   documentCacheMetadata
}

type documentCacheMetadata struct {
	DocumentID   string
	Path         string
	UpdateID     int64
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
	BackendUpdateID     int64
	AppliedSeq          int64
	ProjectedSeq        int64
	ProjectedTextSHA256 string
	ProjectedTextLen    int
	ProjectionKnown     bool
	UpdatedAt           time.Time
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
			backend_update_id integer not null default 0,
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
			source text not null check (source in ('local', 'remote')),
			backend_update_id integer,
			actor_id text,
			actor_type text,
			created_at integer not null,
			unique (document_id, update_sha256)
		)`,
		`create index if not exists crdt_updates_by_doc_seq on crdt_updates(document_id, seq)`,
		`create index if not exists crdt_updates_backend_id on crdt_updates(document_id, backend_update_id)`,
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
		UpdateID:     metadata.UpdateID,
	}, nil
}

func (c *workspaceStore) storeDoc(documentID, path string, updateID int64, doc *crdt.Doc) error {
	if c == nil || documentID == "" || doc == nil {
		return nil
	}
	entry := c.entryFor(documentID)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	metadata := documentCacheMetadata{DocumentID: documentID, Path: path, UpdateID: updateID, UpdatedAt: time.Now().UTC()}
	return c.storeDocLocked(entry, metadata, doc)
}

func (c *workspaceStore) storeDocLocked(entry *documentCacheEntry, metadata documentCacheMetadata, doc *crdt.Doc) error {
	if c == nil || doc == nil || metadata.DocumentID == "" {
		return nil
	}
	state := doc.EncodeStateAsUpdate()
	now := time.Now().UTC()
	return c.withTx(func(tx *sql.Tx) error {
		if _, err := c.ensureDocumentTx(tx, metadata.DocumentID, metadata.Path, metadata.UpdateID, now); err != nil {
			return err
		}
		seq, _, err := c.insertCRDTUpdateTx(tx, metadata.DocumentID, state, "local", metadata.UpdateID, "", "", now)
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
		}
		metadata.UpdatedAt = now
		if entry != nil {
			entry.metadata = metadata
		}
		return nil
	})
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
		if _, err := c.ensureDocumentTx(tx, documentID, path, 0, now); err != nil {
			return err
		}
		_, ok, err := c.insertCRDTUpdateTx(tx, documentID, update, "remote", 0, "", "", now)
		if err != nil {
			return err
		}
		inserted = ok
		return nil
	})
	return inserted, err
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
	row, err := c.ensureDocument(documentID, path, 0)
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
	row, err := c.ensureDocument(documentID, "", 0)
	if err != nil {
		return 0, err
	}
	var count int
	err = c.db.QueryRow(`select count(*) from crdt_updates where document_id = ? and source = 'remote' and seq > ?`, documentID, row.AppliedSeq).Scan(&count)
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

func (c *workspaceStore) applyPendingRemoteUpdatesLocked(_ *documentCacheEntry, documentID string, doc *crdt.Doc) (int, error) {
	if c == nil || doc == nil || documentID == "" {
		return 0, nil
	}
	row, err := c.ensureDocument(documentID, "", 0)
	if err != nil {
		return 0, err
	}
	rows, err := c.db.Query(`select seq, update_bytes from crdt_updates where document_id = ? and seq > ? order by seq`, documentID, row.AppliedSeq)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	maxSeq := row.AppliedSeq
	count := 0
	for rows.Next() {
		var seq int64
		var update []byte
		if err := rows.Scan(&seq, &update); err != nil {
			return count, err
		}
		if len(update) == 0 {
			continue
		}
		if err := crdt.ApplyUpdateV1(doc, update, "remote-reconcile"); err != nil {
			return count, err
		}
		if seq > maxSeq {
			maxSeq = seq
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return count, err
	}
	if count == 0 {
		return 0, nil
	}
	_, err = c.db.Exec(`update documents set applied_seq = ?, updated_at = ? where document_id = ?`, maxSeq, unixNano(time.Now().UTC()), documentID)
	return count, err
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
		row, err := c.ensureDocumentTx(tx, documentID, path, 0, now)
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

func (c *workspaceStore) acceptOutboxUpdateLocked(_ *documentCacheEntry, documentID, path string, record *outboxUpdateRecord, backendUpdateID int64) error {
	if c == nil || documentID == "" || record == nil || len(record.Update) == 0 {
		return nil
	}
	now := time.Now().UTC()
	return c.withTx(func(tx *sql.Tx) error {
		row, err := c.ensureDocumentTx(tx, documentID, path, backendUpdateID, now)
		if err != nil {
			return err
		}
		seq, _, err := c.insertCRDTUpdateTx(tx, documentID, record.Update, "local", backendUpdateID, record.ActorID, record.ActorType, now)
		if err != nil {
			return err
		}
		if seq > 0 {
			var gapCount int
			if err := tx.QueryRow(`select count(*) from crdt_updates where document_id = ? and seq > ? and seq < ?`, documentID, row.AppliedSeq, seq).Scan(&gapCount); err != nil {
				return err
			}
			if gapCount == 0 {
				if _, err := tx.Exec(`update documents
					set applied_seq = max(applied_seq, ?),
						backend_update_id = case when ? > backend_update_id then ? else backend_update_id end,
						updated_at = ?
					where document_id = ?`, seq, backendUpdateID, backendUpdateID, unixNano(now), documentID); err != nil {
					return err
				}
			} else if _, err := tx.Exec(`update documents
				set backend_update_id = case when ? > backend_update_id then ? else backend_update_id end,
					updated_at = ?
				where document_id = ?`, backendUpdateID, backendUpdateID, unixNano(now), documentID); err != nil {
				return err
			}
		}
		if record.ID != "" {
			_, err = tx.Exec(`delete from content_outbox where id = ?`, record.ID)
		} else {
			_, err = tx.Exec(`delete from content_outbox where id = (
				select id from content_outbox where document_id = ? order by created_at, id limit 1
			)`, documentID)
		}
		return err
	})
}

func (c *workspaceStore) removeDocumentLocked(entry *documentCacheEntry, documentID string) error {
	if c == nil || documentID == "" {
		return nil
	}
	err := c.withTx(func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`delete from thread_outbox where document_id = ?`,
			`delete from content_outbox where document_id = ?`,
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

func (c *workspaceStore) storeProjectedBase(documentID, content string, states ...[]byte) error {
	if c == nil || documentID == "" {
		return nil
	}
	row, err := c.ensureDocument(documentID, "", 0)
	if err != nil {
		return err
	}
	projectedSeq := row.AppliedSeq
	if len(states) > 0 && len(states[0]) > 0 {
		if seq, ok, err := c.findProjectedSeq(documentID, content, states[0]); err != nil {
			return err
		} else if ok {
			projectedSeq = seq
		}
	}
	now := time.Now().UTC()
	_, err = c.db.Exec(`update documents
		set projected_seq = ?,
			projected_text_sha256 = ?,
			projected_text_len = ?,
			projection_known = 1,
			updated_at = ?
		where document_id = ?`, projectedSeq, sha256Hex([]byte(content)), len(content), unixNano(now), documentID)
	return err
}

func (c *workspaceStore) findProjectedSeq(documentID, content string, state []byte) (int64, bool, error) {
	if c == nil || documentID == "" || len(state) == 0 {
		return 0, false, nil
	}
	targetStateHash := sha256Hex(state)
	rows, err := c.db.Query(`select seq, update_bytes from crdt_updates where document_id = ? order by seq`, documentID)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	doc := crdt.New()
	defer doc.Close()
	for rows.Next() {
		var seq int64
		var update []byte
		if err := rows.Scan(&seq, &update); err != nil {
			return 0, false, err
		}
		if len(update) == 0 {
			continue
		}
		if err := crdt.ApplyUpdateV1(doc, update, "projection-seq"); err != nil {
			return 0, false, err
		}
		if doc.GetText("content").ToString() == content && sha256Hex(doc.EncodeStateAsUpdate()) == targetStateHash {
			return seq, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	return 0, false, nil
}

func (c *workspaceStore) loadProjectedBase(documentID string) (string, []byte, bool, error) {
	if c == nil || documentID == "" {
		return "", nil, false, nil
	}
	row, err := c.ensureDocument(documentID, "", 0)
	if err != nil {
		return "", nil, false, err
	}
	if !row.ProjectionKnown {
		return "", nil, false, nil
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
	row, err := c.ensureDocument(documentID, "", 0)
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
		   or exists (select 1 from crdt_updates where crdt_updates.document_id = documents.document_id and crdt_updates.source = 'remote' and crdt_updates.seq > documents.applied_seq)
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

func (c *workspaceStore) ensureDocument(documentID, path string, updateID int64) (documentRow, error) {
	if c == nil || documentID == "" {
		return documentRow{DocumentID: documentID, Path: path}, nil
	}
	var row documentRow
	now := time.Now().UTC()
	err := c.withTx(func(tx *sql.Tx) error {
		next, err := c.ensureDocumentTx(tx, documentID, path, updateID, now)
		if err != nil {
			return err
		}
		row = next
		return nil
	})
	return row, err
}

func (c *workspaceStore) ensureDocumentTx(tx *sql.Tx, documentID, path string, updateID int64, now time.Time) (documentRow, error) {
	if documentID == "" {
		return documentRow{}, errors.New("document id is required")
	}
	if _, err := tx.Exec(`insert into documents (document_id, path, backend_update_id, updated_at)
		values (?, ?, ?, ?)
		on conflict(document_id) do update set
			path = case when excluded.path != '' then excluded.path else documents.path end,
			backend_update_id = case when excluded.backend_update_id > documents.backend_update_id then excluded.backend_update_id else documents.backend_update_id end,
			updated_at = excluded.updated_at`,
		documentID, path, updateID, unixNano(now)); err != nil {
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
	err := tx.QueryRow(`select document_id, path, backend_update_id, applied_seq, projected_seq, projected_text_sha256, projected_text_len, projection_known, updated_at
		from documents where document_id = ?`, documentID).Scan(
		&row.DocumentID, &row.Path, &row.BackendUpdateID, &row.AppliedSeq, &row.ProjectedSeq,
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

func (c *workspaceStore) insertCRDTUpdateTx(tx *sql.Tx, documentID string, update []byte, source string, backendUpdateID int64, actorID, actorType string, now time.Time) (int64, bool, error) {
	if len(update) == 0 {
		return 0, false, nil
	}
	updateHash := sha256Hex(update)
	var backend any
	if backendUpdateID > 0 {
		backend = backendUpdateID
	}
	res, err := tx.Exec(`insert or ignore into crdt_updates (
			document_id, update_sha256, update_bytes, source, backend_update_id, actor_id, actor_type, created_at
		) values (?, ?, ?, ?, ?, ?, ?, ?)`,
		documentID, updateHash, update, source, backend, actorID, actorType, unixNano(now))
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

func (c *workspaceStore) loadDocAtSeq(documentID string, seq int64) (*crdt.Doc, []byte, bool, error) {
	doc := crdt.New()
	if c == nil || documentID == "" || seq <= 0 {
		return doc, nil, false, nil
	}
	rows, err := c.db.Query(`select update_bytes from crdt_updates where document_id = ? and seq <= ? order by seq`, documentID, seq)
	if err != nil {
		doc.Close()
		return nil, nil, false, err
	}
	defer rows.Close()
	known := false
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			doc.Close()
			return nil, nil, false, err
		}
		if len(update) == 0 {
			continue
		}
		if err := crdt.ApplyUpdateV1(doc, update, "sqlite-load"); err != nil {
			doc.Close()
			return nil, nil, false, err
		}
		known = true
	}
	if err := rows.Err(); err != nil {
		doc.Close()
		return nil, nil, false, err
	}
	if !known {
		return doc, nil, false, nil
	}
	return doc, doc.EncodeStateAsUpdate(), true, nil
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
		UpdateID:     r.BackendUpdateID,
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
