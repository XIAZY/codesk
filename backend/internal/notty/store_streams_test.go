package notty

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	crdt "notty/internal/ycrdt"
)

func TestPostgresGenericStreamUpdateDedupeAndRestore(t *testing.T) {
	dsn := postgresTestDSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err := clearNottyTables(db); err != nil {
		t.Fatalf("clear tables: %v", err)
	}

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	rootStreamID, err := store.BootstrapWorkspaceStreams()
	if err != nil {
		t.Fatalf("bootstrap streams: %v", err)
	}
	if rootStreamID == "" {
		t.Fatal("expected root stream id")
	}

	rootHead, err := store.GetStreamHead(rootStreamID)
	if err != nil {
		t.Fatalf("get root stream head: %v", err)
	}
	if rootHead.Kind != StreamKindRoot || rootHead.UpdateID != 0 {
		t.Fatalf("expected empty root head, got %#v", rootHead)
	}

	author := crdt.New(crdt.WithClientID(4101))
	text := author.GetText("content")
	update := captureDocUpdate(t, author, "stream-test", func(txn *crdt.Transaction) {
		text.Insert(txn, 0, "hello generic streams", nil)
	})

	contentStreamID := "content_generic_stream"
	ensureRootContentEntryForTest(t, store, rootStreamID, contentStreamID, "generic.md")
	rootHeadAfterEntry, err := store.GetStreamHead(rootStreamID)
	if err != nil {
		t.Fatalf("get root stream head after root entry: %v", err)
	}
	applied, err := store.ApplyStreamUpdate(contentStreamID, update, OperationMeta{ActorID: "tester", ActorType: "human"})
	if err != nil {
		t.Fatalf("apply stream update: %v", err)
	}
	if !applied.Accepted || !applied.Applied || applied.UpdateID <= 0 || len(applied.StateVector) == 0 {
		t.Fatalf("expected applied stream update, got %#v", applied)
	}

	duplicate, err := store.ApplyStreamUpdate(contentStreamID, update, OperationMeta{ActorID: "tester", ActorType: "human"})
	if err != nil {
		t.Fatalf("apply duplicate stream update: %v", err)
	}
	if !duplicate.Accepted || duplicate.Applied || duplicate.UpdateID != applied.UpdateID {
		t.Fatalf("expected deduped duplicate at update %d, got %#v", applied.UpdateID, duplicate)
	}

	var updateRows int
	if err := db.QueryRow(
		`SELECT COUNT(*)
		   FROM crdt_stream_updates
		  WHERE workspace_id = $1 AND stream_id = $2`,
		"ws_notty",
		contentStreamID,
	).Scan(&updateRows); err != nil {
		t.Fatalf("count stream updates: %v", err)
	}
	if updateRows != 1 {
		t.Fatalf("expected exactly one stored stream update, got %d", updateRows)
	}

	restored, restoredHead, err := store.RestoreStreamDoc(contentStreamID)
	if err != nil {
		t.Fatalf("restore stream doc: %v", err)
	}
	if restoredHead.UpdateID != applied.UpdateID {
		t.Fatalf("expected restored head update %d, got %d", applied.UpdateID, restoredHead.UpdateID)
	}
	if got := restored.GetText("content").ToString(); got != "hello generic streams" {
		t.Fatalf("expected restored content, got %q", got)
	}

	deleteUpdate := captureDocUpdate(t, author, "stream-delete", func(txn *crdt.Transaction) {
		text.Delete(txn, len("hello "), len("generic "))
	})
	deleted, err := store.ApplyStreamUpdate(contentStreamID, deleteUpdate, OperationMeta{ActorID: "tester", ActorType: "human"})
	if err != nil {
		t.Fatalf("apply delete stream update: %v", err)
	}
	if !deleted.Accepted || !deleted.Applied || deleted.UpdateID <= applied.UpdateID {
		t.Fatalf("expected delete update applied after insert, got %#v", deleted)
	}
	restored, _, err = store.RestoreStreamDoc(contentStreamID)
	if err != nil {
		t.Fatalf("restore stream doc after delete: %v", err)
	}
	if got := restored.GetText("content").ToString(); got != "hello streams" {
		t.Fatalf("expected delete-set content, got %q", got)
	}

	rootStreamIDAgain, err := store.BootstrapWorkspaceStreams()
	if err != nil {
		t.Fatalf("bootstrap streams again: %v", err)
	}
	if rootStreamIDAgain != rootStreamID {
		t.Fatalf("bootstrap must preserve root stream id, got %q then %q", rootStreamID, rootStreamIDAgain)
	}
	headAfterBootstrap, err := store.GetStreamHead(rootStreamID)
	if err != nil {
		t.Fatalf("get root head after bootstrap: %v", err)
	}
	if headAfterBootstrap.UpdateID != rootHeadAfterEntry.UpdateID {
		t.Fatalf("bootstrap must not reset root stream head, got update %d want %d", headAfterBootstrap.UpdateID, rootHeadAfterEntry.UpdateID)
	}
}

func TestPostgresGenericStreamCheckpointTailRestore(t *testing.T) {
	dsn := postgresTestDSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err := clearNottyTables(db); err != nil {
		t.Fatalf("clear tables: %v", err)
	}

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	rootStreamID, err := store.BootstrapWorkspaceStreams()
	if err != nil {
		t.Fatalf("bootstrap streams: %v", err)
	}
	streamID := "content_checkpoint_tail"
	ensureRootContentEntryForTest(t, store, rootStreamID, streamID, "checkpoint-tail.md")
	author := crdt.New(crdt.WithClientID(4102))
	text := author.GetText("content")
	for _, value := range []string{"a", "b", "c"} {
		update := captureDocUpdate(t, author, "stream-tail", func(txn *crdt.Transaction) {
			text.Insert(txn, text.LenInTxn(txn), value, nil)
		})
		if _, err := store.ApplyStreamUpdate(streamID, update, OperationMeta{ActorID: "tester", ActorType: "human"}); err != nil {
			t.Fatalf("apply update %q: %v", value, err)
		}
	}

	var checkpointRows int
	if err := db.QueryRow(
		`SELECT COUNT(*)
		   FROM crdt_stream_checkpoints
		  WHERE workspace_id = $1 AND stream_id = $2`,
		"ws_notty",
		streamID,
	).Scan(&checkpointRows); err != nil {
		t.Fatalf("count stream checkpoints: %v", err)
	}
	if checkpointRows == 0 {
		t.Fatal("expected at least one stream checkpoint")
	}

	restored, head, err := store.RestoreStreamDoc(streamID)
	if err != nil {
		t.Fatalf("restore stream doc: %v", err)
	}
	if head.UpdateID <= 0 {
		t.Fatalf("expected stream head update id, got %#v", head)
	}
	if got := restored.GetText("content").ToString(); got != "abc" {
		t.Fatalf("expected checkpoint plus tail restore to equal full replay, got %q", got)
	}
}

func TestPostgresStreamUpdateRejectsUnreferencedContentStream(t *testing.T) {
	dsn := postgresTestDSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err := clearNottyTables(db); err != nil {
		t.Fatalf("clear tables: %v", err)
	}

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()
	if _, err := store.BootstrapWorkspaceStreams(); err != nil {
		t.Fatalf("bootstrap streams: %v", err)
	}

	author := crdt.New(crdt.WithClientID(4103))
	text := author.GetText("content")
	update := captureDocUpdate(t, author, "unknown-stream", func(txn *crdt.Transaction) {
		text.Insert(txn, 0, "unauthorized", nil)
	})
	if _, err := store.ApplyStreamUpdate("content_not_in_root", update, OperationMeta{ActorID: "tester", ActorType: "human"}); err == nil {
		t.Fatal("expected unreferenced content stream update to be rejected")
	}
	if _, err := store.GetStreamHead("content_not_in_root"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unreferenced update should not create stream head, got %v", err)
	}
}

func TestPostgresStreamSyncAuthorizationUsesRootManifest(t *testing.T) {
	dsn := postgresTestDSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err := clearNottyTables(db); err != nil {
		t.Fatalf("clear tables: %v", err)
	}

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()
	rootStreamID, err := store.BootstrapWorkspaceStreams()
	if err != nil {
		t.Fatalf("bootstrap streams: %v", err)
	}
	if _, _, err := store.EncodeStreamSyncUpdates(rootStreamID, nil); err != nil {
		t.Fatalf("root stream sync should be allowed: %v", err)
	}
	if _, _, err := store.EncodeStreamSyncUpdates("content_not_in_root", nil); err == nil {
		t.Fatal("expected unreferenced content stream sync to be rejected")
	}

	contentStreamID := "content_sync_authorized"
	ensureRootContentEntryForTest(t, store, rootStreamID, contentStreamID, "sync-authorized.md")
	if _, _, err := store.EncodeStreamSyncUpdates(contentStreamID, nil); err != nil {
		t.Fatalf("live referenced content stream sync should be allowed: %v", err)
	}

	rootDoc, _, err := store.RestoreStreamDoc(rootStreamID)
	if err != nil {
		t.Fatalf("restore root stream: %v", err)
	}
	defer rootDoc.Close()
	tombstoneUpdate, err := ApplyRootIntents(rootDoc, []RootIntent{{
		Type:      "tombstone",
		EntryID:   contentStreamID,
		Tombstone: &RootTombstone{ActorID: "tester", ActorType: "human", At: "2026-05-24T00:00:00Z"},
	}})
	if err != nil {
		t.Fatalf("build root tombstone update: %v", err)
	}
	if _, err := store.ApplyStreamUpdate(rootStreamID, tombstoneUpdate, OperationMeta{ActorID: "tester", ActorType: "human"}); err != nil {
		t.Fatalf("apply root tombstone update: %v", err)
	}
	if _, _, err := store.EncodeStreamSyncUpdates(contentStreamID, nil); err == nil {
		t.Fatal("expected tombstoned content stream sync to be rejected")
	}
}

func TestPostgresRestoreStreamDocUsesStreamGUID(t *testing.T) {
	dsn := postgresTestDSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err := clearNottyTables(db); err != nil {
		t.Fatalf("clear tables: %v", err)
	}

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()
	rootStreamID, err := store.BootstrapWorkspaceStreams()
	if err != nil {
		t.Fatalf("bootstrap streams: %v", err)
	}
	doc, _, err := store.RestoreStreamDoc(rootStreamID)
	if err != nil {
		t.Fatalf("restore root stream: %v", err)
	}
	defer doc.Close()
	if got := doc.GUID(); got != rootStreamID {
		t.Fatalf("restored stream doc GUID = %q, want %q", got, rootStreamID)
	}
}

func TestPostgresRootStreamValidationRejectsMalformedManifest(t *testing.T) {
	dsn := postgresTestDSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err := clearNottyTables(db); err != nil {
		t.Fatalf("clear tables: %v", err)
	}

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	rootStreamID, err := store.BootstrapWorkspaceStreams()
	if err != nil {
		t.Fatalf("bootstrap streams: %v", err)
	}
	author := crdt.New(crdt.WithClientID(4104))
	text := author.GetText(RootManifestTextName)
	update := captureDocUpdate(t, author, "bad-root", func(txn *crdt.Transaction) {
		text.Insert(txn, 0, "not json", nil)
	})
	if _, err := store.ApplyStreamUpdate(rootStreamID, update, OperationMeta{ActorID: "tester", ActorType: "human"}); err == nil {
		t.Fatal("expected malformed root manifest update to be rejected")
	}

	head, err := store.GetStreamHead(rootStreamID)
	if err != nil {
		t.Fatalf("get root head: %v", err)
	}
	if head.UpdateID != 0 {
		t.Fatalf("rejected root update must not advance head, got %d", head.UpdateID)
	}
}

func TestPostgresRootUpdateRejectsLegacyEntriesByIdWrite(t *testing.T) {
	dsn := postgresTestDSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err := clearNottyTables(db); err != nil {
		t.Fatalf("clear tables: %v", err)
	}

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	rootStreamID, err := store.BootstrapWorkspaceStreams()
	if err != nil {
		t.Fatalf("bootstrap streams: %v", err)
	}
	rootDoc, _, err := store.RestoreStreamDoc(rootStreamID)
	if err != nil {
		t.Fatalf("restore root stream: %v", err)
	}
	defer rootDoc.Close()

	entriesMap := rootDoc.GetMap(RootManifestMapName)
	update := captureDocUpdate(t, rootDoc, "legacy-entries-write", func(txn *crdt.Transaction) {
		payload, err := json.Marshal(RootEntry{
			ID:              "doc_legacy",
			Kind:            RootEntryKindFile,
			Loc:             NewRootLocation(RootEntryID, "legacy.md"),
			ContentStreamID: "doc_legacy",
		})
		if err != nil {
			t.Fatalf("marshal legacy entry: %v", err)
		}
		if err := entriesMap.InsertJSON(txn, "doc_legacy", string(payload)); err != nil {
			t.Fatalf("write legacy entriesById: %v", err)
		}
	})
	if _, err := store.ApplyStreamUpdate(rootStreamID, update, OperationMeta{ActorID: "tester", ActorType: "human"}); err == nil || !strings.Contains(err.Error(), "legacy root storage is read-only") {
		t.Fatalf("expected legacy entriesById write rejection, got %v", err)
	}
	head, err := store.GetStreamHead(rootStreamID)
	if err != nil {
		t.Fatalf("get root head: %v", err)
	}
	if head.UpdateID != 0 {
		t.Fatalf("rejected legacy root update must not advance head, got %d", head.UpdateID)
	}
}

func TestPostgresRootUpdateRejectsRootManifestJSONWrite(t *testing.T) {
	dsn := postgresTestDSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err := clearNottyTables(db); err != nil {
		t.Fatalf("clear tables: %v", err)
	}

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()

	rootStreamID, err := store.BootstrapWorkspaceStreams()
	if err != nil {
		t.Fatalf("bootstrap streams: %v", err)
	}
	rootDoc, _, err := store.RestoreStreamDoc(rootStreamID)
	if err != nil {
		t.Fatalf("restore root stream: %v", err)
	}
	defer rootDoc.Close()

	rootText := rootDoc.GetText(RootManifestTextName)
	update := captureDocUpdate(t, rootDoc, "legacy-text-write", func(txn *crdt.Transaction) {
		rootText.Insert(txn, 0, `{"entriesById":{"root":{"id":"root","kind":"dir","loc":null}}}`, nil)
	})
	if _, err := store.ApplyStreamUpdate(rootStreamID, update, OperationMeta{ActorID: "tester", ActorType: "human"}); err == nil || !strings.Contains(err.Error(), "legacy root storage is read-only") {
		t.Fatalf("expected legacy rootManifestJSON write rejection, got %v", err)
	}
	head, err := store.GetStreamHead(rootStreamID)
	if err != nil {
		t.Fatalf("get root head: %v", err)
	}
	if head.UpdateID != 0 {
		t.Fatalf("rejected legacy root update must not advance head, got %d", head.UpdateID)
	}
}

func TestMirrorDocumentCreateDoesNotLeaveContentHeadWhenContentInitFails(t *testing.T) {
	dsn := postgresTestDSN(t)

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err := clearNottyTables(db); err != nil {
		t.Fatalf("clear tables: %v", err)
	}

	store, err := NewStore(dsn)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer store.Close()
	doc := &Document{
		ID:          "doc_bad_content_init",
		Path:        "bad-content.md",
		DesiredPath: "bad-content.md",
		Title:       "bad-content.md",
	}
	if err := store.MirrorDocumentCreateToStreams(doc, "", []byte("not a yjs update"), OperationMeta{ActorID: "tester", ActorType: "human"}); err == nil {
		t.Fatal("expected invalid content init to reject create")
	}
	if _, err := store.GetStreamHead(doc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed create should not leave content stream head, got %v", err)
	}
	rootStreamID, manifest, _, err := store.ReadRootManifestStream()
	if err != nil {
		t.Fatalf("read root manifest stream: %v", err)
	}
	if rootStreamID == "" {
		t.Fatal("expected bootstrapped root stream")
	}
	if _, ok := manifest.EntriesByID[doc.ID]; ok {
		t.Fatalf("failed create should not leave root entry %#v", manifest.EntriesByID[doc.ID])
	}
}

func ensureRootContentEntryForTest(t *testing.T, store *Store, rootStreamID string, streamID string, path string) {
	t.Helper()
	rootDoc, _, err := store.RestoreStreamDoc(rootStreamID)
	if err != nil {
		t.Fatalf("restore root stream: %v", err)
	}
	defer rootDoc.Close()
	update, err := ApplyRootIntents(rootDoc, []RootIntent{{
		Type: "create-file",
		Entry: RootEntry{
			ID:              streamID,
			Kind:            RootEntryKindFile,
			Loc:             NewRootLocation(RootEntryID, path),
			ContentStreamID: streamID,
		},
	}})
	if err != nil {
		t.Fatalf("build root content entry: %v", err)
	}
	if _, err := store.ApplyStreamUpdate(rootStreamID, update, OperationMeta{ActorID: "tester", ActorType: "daemon"}); err != nil {
		t.Fatalf("apply root content entry: %v", err)
	}
}
