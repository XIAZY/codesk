package notty

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
	crdt "notty/internal/ycrdt"
)

func TestUUIDGroup2MigrationConvertsDocumentIDsAndRegeneratesRoot(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)
	createLegacyUUIDGroup1Schema(t, db)
	fixture := seedLegacyUUIDGroup1Graph(t, db)
	tombstonedDocumentID := seedUUIDGroup2LegacyContentDocument(t, db, fixture.old["workspace"], "doc_88888888-8888-8888-8888-888888888888", "docs/old.md")
	seedUUIDGroup2LegacyRootDocument(t, db, fixture, tombstonedDocumentID)
	seedUUIDGroup2OptionalAndDisposableRefs(t, db, fixture)

	contentUpdateBefore := queryBytes(t, db, `SELECT update FROM document_updates WHERE document_id = $1`, fixture.documentID)
	contentCheckpointBefore := queryString(t, db, `SELECT crdt_state FROM document_checkpoints WHERE document_id = $1`, fixture.documentID)

	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("startup migration: %v", err)
	}

	assertColumnType(t, db, "documents", "id", "uuid")
	assertColumnType(t, db, "workspaces", "root_document_id", "uuid")
	assertColumnType(t, db, "document_updates", "document_id", "uuid")
	assertColumnType(t, db, "activities", "document_id", "uuid")
	assertColumnType(t, db, "agent_events", "document_id", "uuid")
	assertColumnType(t, db, "workspace_members", "last_accessed_document_id", "uuid")
	assertColumnType(t, db, "documents", "path", "text")
	assertColumnType(t, db, "documents", "title", "text")
	assertColumnType(t, db, "presences", "file_path", "text")

	contentDocumentID := queryString(t, db, `SELECT new_id::text FROM uuid_migration_map WHERE entity_type = 'documents' AND old_id = $1`, fixture.documentID)
	if contentDocumentID != strings.TrimPrefix(fixture.documentID, "doc_") {
		t.Fatalf("content document mapping = %q, want stripped UUID", contentDocumentID)
	}
	rootDocumentID := queryString(t, db, `SELECT root_document_id::text FROM workspaces WHERE id::text = $1`, fixture.ids["workspace"])
	if rootDocumentID == fixture.rootDocumentID || rootDocumentID == strings.TrimPrefix(fixture.rootDocumentID, "doc_") || !isUUIDString(rootDocumentID) {
		t.Fatalf("root document ID = %q, want fresh UUID", rootDocumentID)
	}

	assertBytes(t, db, `SELECT update FROM document_updates WHERE document_id::text = $1`, contentUpdateBefore, contentDocumentID)
	assertScalar(t, db, `SELECT crdt_state FROM document_checkpoints WHERE document_id::text = $1`, contentCheckpointBefore, contentDocumentID)
	assertScalar(t, db, `SELECT COUNT(*) FROM document_updates WHERE document_id::text = $1`, 0, fixture.rootDocumentID)
	assertScalar(t, db, `SELECT COUNT(*) FROM document_updates WHERE document_id::text = $1`, 1, rootDocumentID)
	assertScalar(t, db, `SELECT hidden FROM documents WHERE id::text = $1`, true, rootDocumentID)

	rootEntries := loadUUIDGroup2RootEntries(t, db, fixture.ids["workspace"], rootDocumentID)
	tombstonedMappedID := queryString(t, db, `SELECT new_id::text FROM uuid_migration_map WHERE entity_type = 'documents' AND old_id = $1`, tombstonedDocumentID)
	assertUUIDGroup2RootEntry(t, rootEntries, contentDocumentID, "docs/spec.md", false)
	assertUUIDGroup2RootEntry(t, rootEntries, tombstonedMappedID, "docs/old.md", true)
	assertUUIDGroup2MigratedRootStoreOperations(t, db, fixture.ids["workspace"], rootDocumentID)

	assertScalar(t, db, `SELECT COUNT(*) FROM presences WHERE document_id::text = $1`, 0, "doc_99999999-9999-9999-9999-999999999999")
	assertScalar(t, db, `SELECT COUNT(*) FROM agent_document_views WHERE document_id::text = $1`, 0, "doc_99999999-9999-9999-9999-999999999999")
	assertScalar(t, db, `SELECT COUNT(*) FROM presences WHERE document_id::text = $1`, 2, contentDocumentID)
	assertScalar(t, db, `SELECT COUNT(*) FROM agent_document_views WHERE document_id::text = $1`, 1, contentDocumentID)
	assertScalar(t, db, `SELECT COUNT(*) FROM activities WHERE document_id IS NULL`, 1)
	assertScalar(t, db, `SELECT COUNT(*) FROM agent_events WHERE document_id IS NULL`, 1)

	if err := VerifyUUIDGroup2Deep(t.Context(), db); err != nil {
		t.Fatalf("verify group2 deep: %v", err)
	}
	if err := RunUUIDGroup2Migration(t.Context(), db); err != nil {
		t.Fatalf("rerun group2 migration: %v", err)
	}
	if err := VerifyUUIDGroup2BootShape(t.Context(), db); err != nil {
		t.Fatalf("verify group2 boot shape: %v", err)
	}
}

func TestUUIDGroup2MigrationFailsClosedOnMalformedDocumentID(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)
	createLegacyUUIDGroup1Schema(t, db)
	fixture := seedLegacyUUIDGroup1Graph(t, db)
	seedUUIDGroup2LegacyRootDocument(t, db, fixture, "")
	if err := RunUUIDGroup1Migration(t.Context(), db); err != nil {
		t.Fatalf("run group1 migration: %v", err)
	}
	mustExec(t, db, `INSERT INTO documents (workspace_id, id, path, title, client_id_seed) VALUES ($1::uuid, $2, $3, $4, $5)`,
		fixture.ids["workspace"], "doc_not-a-uuid", "bad.md", "Bad", int64(77))
	malformedUpdate := []byte{0xba, 0xdd, 0x0c}
	var malformedUpdateID int64
	if err := db.QueryRow(`INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type) VALUES ($1::uuid, $2, $3, NULL, 'system') RETURNING id`,
		fixture.ids["workspace"], "doc_not-a-uuid", malformedUpdate).Scan(&malformedUpdateID); err != nil {
		t.Fatalf("insert malformed document update: %v", err)
	}
	mustExec(t, db, `INSERT INTO document_heads (workspace_id, document_id, state_vector, update_id) VALUES ($1::uuid, $2, $3, $4)`,
		fixture.ids["workspace"], "doc_not-a-uuid", "malformed-head", malformedUpdateID)
	mustExec(t, db, `INSERT INTO document_checkpoints (workspace_id, document_id, update_id, crdt_state, state_vector) VALUES ($1::uuid, $2, $3, $4, $5)`,
		fixture.ids["workspace"], "doc_not-a-uuid", malformedUpdateID, "malformed-checkpoint", "malformed-csv")

	err = RunUUIDGroup2Migration(t.Context(), db)
	if err == nil {
		t.Fatal("expected group2 migration to fail on malformed document id")
	}
	if !strings.Contains(err.Error(), "doc_not-a-uuid") {
		t.Fatalf("migration error = %v, want malformed document id", err)
	}
	assertColumnType(t, db, "documents", "id", "text")
	assertScalar(t, db, `SELECT COUNT(*) FROM documents WHERE id = 'doc_not-a-uuid'`, 1)
	assertBytes(t, db, `SELECT update FROM document_updates WHERE document_id = 'doc_not-a-uuid'`, malformedUpdate)
	assertScalar(t, db, `SELECT state_vector FROM document_heads WHERE document_id = 'doc_not-a-uuid'`, "malformed-head")
	assertScalar(t, db, `SELECT crdt_state FROM document_checkpoints WHERE document_id = 'doc_not-a-uuid'`, "malformed-checkpoint")
}

func TestUUIDGroup2MigrationFailsClosedOnMalformedRootDocumentID(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)
	createLegacyUUIDGroup1Schema(t, db)
	fixture := seedLegacyUUIDGroup1Graph(t, db)
	seedUUIDGroup2LegacyRootDocument(t, db, fixture, "")
	if err := RunUUIDGroup1Migration(t.Context(), db); err != nil {
		t.Fatalf("run group1 migration: %v", err)
	}
	mustExec(t, db, `UPDATE documents SET id = $1 WHERE workspace_id = $2 AND id = $3`, "bad_root", fixture.ids["workspace"], fixture.rootDocumentID)
	mustExec(t, db, `UPDATE workspaces SET root_document_id = $1 WHERE id = $2`, "bad_root", fixture.ids["workspace"])

	err = RunUUIDGroup2Migration(t.Context(), db)
	if err == nil {
		t.Fatal("expected group2 migration to fail on malformed root document id")
	}
	if !strings.Contains(err.Error(), "bad_root") {
		t.Fatalf("migration error = %v, want malformed root id", err)
	}
	assertColumnType(t, db, "workspaces", "root_document_id", "text")
	assertScalar(t, db, `SELECT root_document_id FROM workspaces`, "bad_root")
}

func TestUUIDGroup2MigrationFailsClosedOnDataBearingOrphanDocumentRef(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)
	createLegacyUUIDGroup1Schema(t, db)
	fixture := seedLegacyUUIDGroup1Graph(t, db)
	seedUUIDGroup2LegacyRootDocument(t, db, fixture, "")
	if err := RunUUIDGroup1Migration(t.Context(), db); err != nil {
		t.Fatalf("run group1 migration: %v", err)
	}
	mustExec(t, db, `UPDATE activities SET document_id = $1`, "doc_99999999-9999-9999-9999-999999999999")

	err = RunUUIDGroup2Migration(t.Context(), db)
	if err == nil {
		t.Fatal("expected group2 migration to fail on data-bearing orphan document ref")
	}
	if !strings.Contains(err.Error(), "activities.document_id") {
		t.Fatalf("migration error = %v, want activities.document_id", err)
	}
	assertColumnType(t, db, "activities", "document_id", "text")
	assertScalar(t, db, `SELECT document_id FROM activities`, "doc_99999999-9999-9999-9999-999999999999")
}

func TestUUIDGroup2MigrationDeletesDeletedParentThreadSubtree(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)
	createLegacyUUIDGroup1Schema(t, db)
	fixture := seedLegacyUUIDGroup1Graph(t, db)
	seedUUIDGroup2LegacyRootDocument(t, db, fixture, "")

	orphanDocumentID := "doc_8ceb03ba-1d8a-438f-91a3-e526388eb005"
	orphanThreadID := "thread_88888888-8888-8888-8888-888888888881"
	orphanMessageAID := "threadmsg_88888888-8888-8888-8888-888888888882"
	orphanMessageBID := "threadmsg_88888888-8888-8888-8888-888888888883"
	orphanThreadUUID := strings.TrimPrefix(orphanThreadID, "thread_")
	mustExec(t, db,
		`INSERT INTO threads (workspace_id, id, document_id, created_by_id, created_by_type, title, status, created_by_handle, created_by_name)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		fixture.old["workspace"], orphanThreadID, orphanDocumentID, fixture.old["user"], "human", "Deleted Parent Thread", "open", "owner", "Owner")
	mustExec(t, db,
		`INSERT INTO thread_messages (workspace_id, id, thread_id, author_id, author_type, author_handle, author_name, body, kind)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9),
		        ($1, $10, $3, $11, $12, $13, $14, $15, $9)`,
		fixture.old["workspace"], orphanMessageAID, orphanThreadID, fixture.old["user"], "human", "owner", "Owner", "orphan message one", "comment",
		orphanMessageBID, fixture.old["agent"], "agent", "agent", "Agent", "orphan message two")
	mustExec(t, db,
		`INSERT INTO thread_participants (workspace_id, thread_id, participant_id)
		 VALUES ($1, $2, $3), ($1, $2, $4)`,
		fixture.old["workspace"], orphanThreadID, fixture.old["user"], fixture.old["agent"])

	logs := authTestCaptureLogs(t)
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("startup migration: %v", err)
	}

	logLine := logs.waitContains(t,
		"uuid group2 deleting deleted-parent thread",
		"workspace_id="+fixture.ids["workspace"],
		"thread_id="+orphanThreadUUID,
		"document_id="+orphanDocumentID,
		"title=\"Deleted Parent Thread\"",
		"message_count=2",
		"participant_count=2",
	)
	if strings.Contains(logLine, fixture.documentID) {
		t.Fatalf("deleted-parent audit log included reachable document %q: %s", fixture.documentID, logLine)
	}
	assertScalar(t, db, `SELECT COUNT(*) FROM threads WHERE id::text = $1`, int64(0), orphanThreadUUID)
	assertScalar(t, db, `SELECT COUNT(*) FROM thread_messages WHERE thread_id::text = $1`, int64(0), orphanThreadUUID)
	assertScalar(t, db, `SELECT COUNT(*) FROM thread_participants WHERE thread_id::text = $1`, int64(0), orphanThreadUUID)
	assertScalar(t, db, `SELECT COUNT(*) FROM threads WHERE document_id::text = $1`, int64(1), strings.TrimPrefix(fixture.documentID, "doc_"))
	if err := VerifyUUIDGroup2Deep(t.Context(), db); err != nil {
		t.Fatalf("verify group2 deep: %v", err)
	}
}

func TestUUIDGroup2MigrationDeletesDeletedParentDocumentStorage(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)
	createLegacyUUIDGroup1Schema(t, db)
	fixture := seedLegacyUUIDGroup1Graph(t, db)
	seedUUIDGroup2LegacyRootDocument(t, db, fixture, "")

	liveUpdateBefore := queryBytes(t, db, `SELECT update FROM document_updates WHERE document_id = $1 ORDER BY id LIMIT 1`, fixture.documentID)
	liveHeadBefore := queryString(t, db, `SELECT state_vector FROM document_heads WHERE document_id = $1`, fixture.documentID)
	liveCheckpointBefore := queryString(t, db, `SELECT crdt_state FROM document_checkpoints WHERE document_id = $1`, fixture.documentID)

	deletedDocumentID := "doc_3dfd24a0-4eb1-4332-927b-69a710afa49a"
	var deletedUpdateAID int64
	if err := db.QueryRow(`INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type) VALUES ($1, $2, $3, NULL, 'system') RETURNING id`,
		fixture.old["workspace"], deletedDocumentID, []byte{0xde, 0xad, 0x01}).Scan(&deletedUpdateAID); err != nil {
		t.Fatalf("insert deleted-parent update A: %v", err)
	}
	var deletedUpdateBID int64
	if err := db.QueryRow(`INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type) VALUES ($1, $2, $3, NULL, 'system') RETURNING id`,
		fixture.old["workspace"], deletedDocumentID, []byte{0xde, 0xad, 0x02}).Scan(&deletedUpdateBID); err != nil {
		t.Fatalf("insert deleted-parent update B: %v", err)
	}
	mustExec(t, db, `INSERT INTO document_heads (workspace_id, document_id, state_vector, update_id) VALUES ($1, $2, $3, $4)`,
		fixture.old["workspace"], deletedDocumentID, "deleted-head", deletedUpdateBID)
	mustExec(t, db, `INSERT INTO document_checkpoints (workspace_id, document_id, update_id, crdt_state, state_vector) VALUES ($1, $2, $3, $4, $5)`,
		fixture.old["workspace"], deletedDocumentID, deletedUpdateAID, "deleted-checkpoint", "deleted-csv")

	logs := authTestCaptureLogs(t)
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("startup migration: %v", err)
	}

	logs.waitContains(t,
		"uuid group2 deleting deleted-parent document storage",
		"workspace_id="+fixture.ids["workspace"],
		"document_id="+deletedDocumentID,
		"update_count=2",
		"head_count=1",
		"checkpoint_count=1",
	)
	assertScalar(t, db, `SELECT COUNT(*) FROM document_updates WHERE document_id::text = $1`, int64(0), deletedDocumentID)
	assertScalar(t, db, `SELECT COUNT(*) FROM document_heads WHERE document_id::text = $1`, int64(0), deletedDocumentID)
	assertScalar(t, db, `SELECT COUNT(*) FROM document_checkpoints WHERE document_id::text = $1`, int64(0), deletedDocumentID)

	contentDocumentID := strings.TrimPrefix(fixture.documentID, "doc_")
	assertBytes(t, db, `SELECT update FROM document_updates WHERE document_id::text = $1 ORDER BY id LIMIT 1`, liveUpdateBefore, contentDocumentID)
	assertScalar(t, db, `SELECT state_vector FROM document_heads WHERE document_id::text = $1`, liveHeadBefore, contentDocumentID)
	assertScalar(t, db, `SELECT crdt_state FROM document_checkpoints WHERE document_id::text = $1`, liveCheckpointBefore, contentDocumentID)
	if err := VerifyUUIDGroup2Deep(t.Context(), db); err != nil {
		t.Fatalf("verify group2 deep: %v", err)
	}
}

func TestUUIDGroup2DeletedParentCleanupKeepsLegacyThreadRefToNativeDocument(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)
	createLegacyUUIDGroup1Schema(t, db)
	fixture := seedLegacyUUIDGroup1Graph(t, db)
	if err := RunUUIDGroup1Migration(t.Context(), db); err != nil {
		t.Fatalf("run group1 migration: %v", err)
	}
	mustExec(t, db,
		`ALTER TABLE documents
		 ALTER COLUMN id TYPE UUID
		 USING regexp_replace(id, '^doc_', '')::uuid`)

	orphanThreadID := "88888888-8888-8888-8888-888888888881"
	orphanMessageID := "88888888-8888-8888-8888-888888888882"
	mustExec(t, db,
		`INSERT INTO threads (workspace_id, id, document_id, created_by_id, created_by_type, title, status, created_by_handle, created_by_name)
		 VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5, $6, $7, $8, $9)`,
		fixture.ids["workspace"], orphanThreadID, "doc_99999999-9999-9999-9999-999999999999", fixture.ids["user"], "human", "Missing Native Parent", "open", "owner", "Owner")
	mustExec(t, db,
		`INSERT INTO thread_messages (workspace_id, id, thread_id, author_id, author_type, author_handle, author_name, body, kind)
		 VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8, $9)`,
		fixture.ids["workspace"], orphanMessageID, orphanThreadID, fixture.ids["user"], "human", "owner", "Owner", "delete me", "comment")
	mustExec(t, db,
		`INSERT INTO thread_participants (workspace_id, thread_id, participant_id)
		 VALUES ($1::uuid, $2::uuid, $3::uuid)`,
		fixture.ids["workspace"], orphanThreadID, fixture.ids["user"])

	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin cleanup tx: %v", err)
	}
	if err := deleteUUIDGroup2DeletedParentThreadSubtrees(t.Context(), tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("deleted-parent cleanup: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit cleanup tx: %v", err)
	}

	assertScalar(t, db, `SELECT COUNT(*) FROM threads WHERE id::text = $1 AND document_id = $2`, int64(1), fixture.ids["thread"], fixture.documentID)
	assertScalar(t, db, `SELECT COUNT(*) FROM thread_messages WHERE thread_id::text = $1`, int64(1), fixture.ids["thread"])
	assertScalar(t, db, `SELECT COUNT(*) FROM thread_participants WHERE thread_id::text = $1`, int64(2), fixture.ids["thread"])
	assertScalar(t, db, `SELECT COUNT(*) FROM threads WHERE id::text = $1`, int64(0), orphanThreadID)
	assertScalar(t, db, `SELECT COUNT(*) FROM thread_messages WHERE thread_id::text = $1`, int64(0), orphanThreadID)
	assertScalar(t, db, `SELECT COUNT(*) FROM thread_participants WHERE thread_id::text = $1`, int64(0), orphanThreadID)
}

func TestUUIDGroup2DeletedParentCleanupKeepsBareThreadRefToLegacyDocument(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)
	createLegacyUUIDGroup1Schema(t, db)
	fixture := seedLegacyUUIDGroup1Graph(t, db)
	if err := RunUUIDGroup1Migration(t.Context(), db); err != nil {
		t.Fatalf("run group1 migration: %v", err)
	}

	bareDocumentID := strings.TrimPrefix(fixture.documentID, "doc_")
	mustExec(t, db,
		`UPDATE threads
		    SET document_id = $1
		  WHERE id::text = $2`,
		bareDocumentID, fixture.ids["thread"])

	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin cleanup tx: %v", err)
	}
	if err := deleteUUIDGroup2DeletedParentThreadSubtrees(t.Context(), tx); err != nil {
		_ = tx.Rollback()
		t.Fatalf("deleted-parent cleanup: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit cleanup tx: %v", err)
	}

	assertScalar(t, db, `SELECT COUNT(*) FROM threads WHERE id::text = $1 AND document_id = $2`, int64(1), fixture.ids["thread"], bareDocumentID)
	assertScalar(t, db, `SELECT COUNT(*) FROM thread_messages WHERE thread_id::text = $1`, int64(1), fixture.ids["thread"])
	assertScalar(t, db, `SELECT COUNT(*) FROM thread_participants WHERE thread_id::text = $1`, int64(2), fixture.ids["thread"])
}

func TestUUIDGroup2MigrationFailsClosedOnExistingDocumentMappingConflict(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)
	createLegacyUUIDGroup1Schema(t, db)
	fixture := seedLegacyUUIDGroup1Graph(t, db)
	seedUUIDGroup2LegacyRootDocument(t, db, fixture, "")
	if err := RunUUIDGroup1Migration(t.Context(), db); err != nil {
		t.Fatalf("run group1 migration: %v", err)
	}
	wrongMapping := "99999999-9999-9999-9999-999999999999"
	mustExec(t, db,
		`INSERT INTO uuid_migration_map (entity_type, old_id, new_id) VALUES ($1, $2, $3::uuid)`,
		uuidGroup2DocumentEntity, fixture.documentID, wrongMapping)

	err = RunUUIDGroup2Migration(t.Context(), db)
	if err == nil {
		t.Fatal("expected group2 migration to fail on existing document mapping conflict")
	}
	if !strings.Contains(err.Error(), "already points") || !strings.Contains(err.Error(), fixture.documentID) {
		t.Fatalf("migration error = %v, want mapping conflict for existing document", err)
	}
	assertColumnType(t, db, "threads", "document_id", "text")
	assertScalar(t, db, `SELECT COUNT(*) FROM threads WHERE document_id = $1`, int64(1), fixture.documentID)
	assertScalar(t, db, `SELECT COUNT(*) FROM thread_messages WHERE thread_id::text = $1`, int64(1), fixture.ids["thread"])
	assertScalar(t, db, `SELECT COUNT(*) FROM thread_participants WHERE thread_id::text = $1`, int64(2), fixture.ids["thread"])
}

func TestUUIDGroup2MigrationFailsClosedOnRootEntryPointingAtRootDocument(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)
	createLegacyUUIDGroup1Schema(t, db)
	fixture := seedLegacyUUIDGroup1Graph(t, db)
	seedUUIDGroup2LegacyRootDocumentEntries(t, db, fixture, []uuidGroup2RootEntry{{
		EntryID:           fixture.rootDocumentID,
		ContentDocumentID: fixture.rootDocumentID,
		Name:              "docs/root-self.md",
		Deleted:           false,
	}})
	if err := RunUUIDGroup1Migration(t.Context(), db); err != nil {
		t.Fatalf("run group1 migration: %v", err)
	}

	err = RunUUIDGroup2Migration(t.Context(), db)
	if err == nil {
		t.Fatal("expected group2 migration to fail on root entry pointing at root document")
	}
	if !strings.Contains(err.Error(), "root or hidden document") {
		t.Fatalf("migration error = %v, want root/hidden document invariant", err)
	}
	assertColumnType(t, db, "documents", "id", "text")
	assertScalar(t, db, `SELECT COUNT(*) FROM document_updates WHERE document_id = $1`, 1, fixture.rootDocumentID)
}

func seedUUIDGroup2LegacyContentDocument(t *testing.T, db *sql.DB, workspaceID, documentID, path string) string {
	t.Helper()
	update := []byte{0x09, 0x08, 0x07}
	mustExec(t, db, `INSERT INTO documents (workspace_id, id, path, title, hidden, client_id_seed) VALUES ($1, $2, $3, $4, false, $5)`,
		workspaceID, documentID, path, path, int64(43))
	var updateID int64
	if err := db.QueryRow(`INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		workspaceID, documentID, update, "system", "system").Scan(&updateID); err != nil {
		t.Fatalf("insert content update: %v", err)
	}
	mustExec(t, db, `INSERT INTO document_heads (workspace_id, document_id, state_vector, update_id) VALUES ($1, $2, $3, $4)`,
		workspaceID, documentID, "sv-"+documentID, updateID)
	mustExec(t, db, `INSERT INTO document_checkpoints (workspace_id, document_id, update_id, crdt_state, state_vector) VALUES ($1, $2, $3, $4, $5)`,
		workspaceID, documentID, updateID, "checkpoint-"+documentID, "csv-"+documentID)
	return documentID
}

func seedUUIDGroup2LegacyRootDocument(t *testing.T, db *sql.DB, fixture legacyUUIDGroup1Fixture, tombstonedDocumentID string) {
	t.Helper()
	entries := []uuidGroup2RootEntry{{
		EntryID:           fixture.documentID,
		ContentDocumentID: fixture.documentID,
		Name:              "docs/spec.md",
		Deleted:           false,
	}}
	if tombstonedDocumentID != "" {
		entries = append(entries, uuidGroup2RootEntry{
			EntryID:           tombstonedDocumentID,
			ContentDocumentID: tombstonedDocumentID,
			Name:              "docs/old.md",
			Deleted:           true,
		})
	}
	seedUUIDGroup2LegacyRootDocumentEntries(t, db, fixture, entries)
}

func seedUUIDGroup2LegacyRootDocumentEntries(t *testing.T, db *sql.DB, fixture legacyUUIDGroup1Fixture, entries []uuidGroup2RootEntry) {
	t.Helper()
	update, crdtState, stateVector, err := buildUUIDGroup2RootUpdate(99, entries)
	if err != nil {
		t.Fatalf("build legacy root update: %v", err)
	}
	mustExec(t, db, `INSERT INTO documents (workspace_id, id, path, title, hidden, client_id_seed) VALUES ($1, $2, $3, $4, true, $5)`,
		fixture.old["workspace"], fixture.rootDocumentID, legacyRootDocumentPath, legacyRootDocumentTitle, int64(99))
	var updateID int64
	if err := db.QueryRow(`INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type) VALUES ($1, $2, $3, NULL, 'system') RETURNING id`,
		fixture.old["workspace"], fixture.rootDocumentID, update).Scan(&updateID); err != nil {
		t.Fatalf("insert root update: %v", err)
	}
	mustExec(t, db, `INSERT INTO document_heads (workspace_id, document_id, state_vector, update_id) VALUES ($1, $2, $3, $4)`,
		fixture.old["workspace"], fixture.rootDocumentID, stateVector, updateID)
	mustExec(t, db, `INSERT INTO document_checkpoints (workspace_id, document_id, update_id, crdt_state, state_vector) VALUES ($1, $2, $3, $4, $5)`,
		fixture.old["workspace"], fixture.rootDocumentID, updateID, crdtState, stateVector)
}

func seedUUIDGroup2OptionalAndDisposableRefs(t *testing.T, db *sql.DB, fixture legacyUUIDGroup1Fixture) {
	t.Helper()
	orphanDocumentID := "doc_99999999-9999-9999-9999-999999999999"
	contentDocumentID := strings.TrimPrefix(fixture.documentID, "doc_")
	mustExec(t, db, `INSERT INTO presences (workspace_id, actor_id, actor_type, document_id, file_path, mode, activity) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		fixture.old["workspace"], fixture.old["agent"], "agent", orphanDocumentID, "missing.md", "editing", "orphan")
	mustExec(t, db, `INSERT INTO presences (workspace_id, actor_id, actor_type, document_id, file_path, mode, activity) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		fixture.old["workspace"], fixture.old["user"], "human", contentDocumentID, "docs/spec.md", "editing", "valid bare ref")
	mustExec(t, db, `INSERT INTO agent_document_views (workspace_id, agent_id, document_id, update_id, state_vector) VALUES ($1, $2, $3, $4, $5)`,
		fixture.old["workspace"], fixture.old["agent"], orphanDocumentID, int64(1), "missing")
	mustExec(t, db, `INSERT INTO agent_document_views (workspace_id, agent_id, document_id, update_id, state_vector) VALUES ($1, $2, $3, $4, $5)`,
		fixture.old["workspace"], fixture.old["agent"], contentDocumentID, int64(2), "valid")
	mustExec(t, db, `INSERT INTO activities (workspace_id, type, document_id, actor_id, actor_type, summary) VALUES ($1, $2, '', $3, $4, $5)`,
		fixture.old["workspace"], "workspace.updated", fixture.old["user"], "human", "workspace only")
	mustExec(t, db, `INSERT INTO agent_events (workspace_id, id, agent_id, agent_handle, type, status, document_id) VALUES ($1, $2, $3, $4, $5, $6, '')`,
		fixture.old["workspace"], "aevt_"+uuid.NewString(), fixture.old["agent"], "agent", "message", "completed")
}

func loadUUIDGroup2RootEntries(t *testing.T, db *sql.DB, workspaceID, rootDocumentID string) map[string]uuidGroup2RootEntry {
	t.Helper()
	var clientID int64
	var headID int64
	if err := db.QueryRow(`SELECT d.client_id_seed, h.update_id FROM documents d JOIN document_heads h ON h.workspace_id::text = d.workspace_id::text AND h.document_id::text = d.id::text WHERE d.workspace_id::text = $1 AND d.id::text = $2`,
		workspaceID, rootDocumentID).Scan(&clientID, &headID); err != nil {
		t.Fatalf("load root head: %v", err)
	}
	tx, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback()
	doc, err := restoreUUIDGroup2DocumentDoc(t.Context(), tx, workspaceID, rootDocumentID, uint64(clientID), headID)
	if err != nil {
		t.Fatalf("restore root doc: %v", err)
	}
	defer doc.Close()
	entries, err := decodeUUIDGroup2RootEntries(doc)
	if err != nil {
		t.Fatalf("decode root entries: %v", err)
	}
	byID := map[string]uuidGroup2RootEntry{}
	for _, entry := range entries {
		byID[entry.EntryID] = entry
	}
	return byID
}

func assertUUIDGroup2RootEntry(t *testing.T, entries map[string]uuidGroup2RootEntry, documentID, path string, deleted bool) {
	t.Helper()
	entry, ok := entries[documentID]
	if !ok {
		t.Fatalf("missing root entry %s in %#v", documentID, entries)
	}
	if entry.ContentDocumentID != documentID {
		t.Fatalf("root entry %s contentDocumentId = %q", documentID, entry.ContentDocumentID)
	}
	if got := entry.desiredPath(); got != path {
		t.Fatalf("root entry %s path = %q, want %q", documentID, got, path)
	}
	if entry.Deleted != deleted {
		t.Fatalf("root entry %s deleted = %t, want %t", documentID, entry.Deleted, deleted)
	}
}

func assertUUIDGroup2MigratedRootStoreOperations(t *testing.T, db *sql.DB, workspaceID, rootDocumentID string) {
	t.Helper()
	database := &Database{DB: db}
	store, err := NewWorkspaceStore(database, workspaceID, "Migrated Workspace")
	if err != nil {
		t.Fatalf("load migrated workspace store: %v", err)
	}
	snapshot := store.Snapshot()
	if snapshot.RootDocumentID != rootDocumentID {
		t.Fatalf("loaded root document ID = %q, want %q", snapshot.RootDocumentID, rootDocumentID)
	}
	if !store.HasDocument(rootDocumentID) {
		t.Fatalf("loaded store missing root document %q", rootDocumentID)
	}

	rootDoc := crdt.New(crdt.WithClientID(31337))
	defer rootDoc.Close()
	_, rootUpdates, err := store.EncodeDocumentSyncUpdates(rootDocumentID, nil)
	if err != nil {
		t.Fatalf("sync migrated root document: %v", err)
	}
	for _, update := range rootUpdates {
		if err := crdt.ApplyUpdateV1(rootDoc, update, "uuid-group2-root-sync"); err != nil {
			t.Fatalf("apply migrated root sync update: %v", err)
		}
	}

	newDocument, err := store.CreateDocument(CreateDocumentRequest{}, OperationMeta{ActorID: "owner", ActorType: "human", Source: "uuid-group2-root-test"})
	if err != nil {
		t.Fatalf("create post-migration document: %v", err)
	}
	if !isUUIDString(newDocument.ID) || strings.HasPrefix(newDocument.ID, "doc_") {
		t.Fatalf("new post-migration document ID = %q, want bare UUID", newDocument.ID)
	}

	upsert, err := upsertRootEntryForTest(rootDoc, newDocument.ID, "docs/new.md")
	if err != nil {
		t.Fatalf("build migrated root upsert: %v", err)
	}
	if _, err := store.ApplyCRDTUpdate(rootDocumentID, upsert, OperationMeta{ActorID: "owner", ActorType: "human", Source: "uuid-group2-root-test"}); err != nil {
		t.Fatalf("apply migrated root upsert: %v", err)
	}

	move, err := upsertRootEntryForTest(rootDoc, newDocument.ID, "docs/moved.md")
	if err != nil {
		t.Fatalf("build migrated root move: %v", err)
	}
	if _, err := store.ApplyCRDTUpdate(rootDocumentID, move, OperationMeta{ActorID: "owner", ActorType: "human", Source: "uuid-group2-root-test"}); err != nil {
		t.Fatalf("apply migrated root move: %v", err)
	}

	tombstone, err := tombstoneRootEntryForTest(rootDoc, newDocument.ID)
	if err != nil {
		t.Fatalf("build migrated root tombstone: %v", err)
	}
	if _, err := store.ApplyCRDTUpdate(rootDocumentID, tombstone, OperationMeta{ActorID: "owner", ActorType: "human", Source: "uuid-group2-root-test"}); err != nil {
		t.Fatalf("apply migrated root tombstone: %v", err)
	}

	reloaded, err := NewWorkspaceStore(database, workspaceID, "Migrated Workspace")
	if err != nil {
		t.Fatalf("reload migrated workspace store: %v", err)
	}
	reloadedRoot := crdt.New(crdt.WithClientID(31338))
	defer reloadedRoot.Close()
	_, reloadedUpdates, err := reloaded.EncodeDocumentSyncUpdates(rootDocumentID, nil)
	if err != nil {
		t.Fatalf("sync reloaded migrated root document: %v", err)
	}
	for _, update := range reloadedUpdates {
		if err := crdt.ApplyUpdateV1(reloadedRoot, update, "uuid-group2-root-reload"); err != nil {
			t.Fatalf("apply reloaded migrated root sync update: %v", err)
		}
	}
	if path, deleted := rootEntryForDocumentForTest(t, reloadedRoot, newDocument.ID); path != "docs/moved.md" || !deleted {
		t.Fatalf("reloaded migrated root entry = path %q deleted %v, want docs/moved.md true", path, deleted)
	}
}

func queryString(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var got string
	if err := db.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatalf("query string %q: %v", query, err)
	}
	return got
}

func queryBytes(t *testing.T, db *sql.DB, query string, args ...any) []byte {
	t.Helper()
	var got []byte
	if err := db.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatalf("query bytes %q: %v", query, err)
	}
	return got
}
