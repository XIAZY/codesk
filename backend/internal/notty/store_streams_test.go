package notty

import (
	"database/sql"
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
	if headAfterBootstrap.UpdateID != 0 {
		t.Fatalf("bootstrap must not reset root stream head, got update %d", headAfterBootstrap.UpdateID)
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

	streamID := "content_checkpoint_tail"
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
