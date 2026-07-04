package notty

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestStripPrefixedUUIDKeepsOriginalUUID(t *testing.T) {
	got, err := stripPrefixedUUID("agent_12345678-1234-1234-1234-123456789abc", "agent_")
	if err != nil {
		t.Fatalf("strip prefixed UUID: %v", err)
	}
	if got != "12345678-1234-1234-1234-123456789abc" {
		t.Fatalf("stripped UUID = %q", got)
	}
	if _, err := stripPrefixedUUID("agent_not-a-uuid", "agent_"); err == nil {
		t.Fatal("expected invalid UUID suffix to fail")
	}
	if _, err := stripPrefixedUUID("user_12345678-1234-1234-1234-123456789abc", "agent_"); err == nil {
		t.Fatal("expected missing prefix to fail")
	}
}

func TestUUIDGroup1ColumnInventoryHasNoDuplicateClassifications(t *testing.T) {
	seen := map[string]UUIDGroup1ColumnBucket{}
	for _, column := range UUIDGroup1ColumnInventory() {
		key := column.Table + "." + column.Column
		if previous, ok := seen[key]; ok {
			t.Fatalf("%s classified twice: %s and %s", key, previous, column.Bucket)
		}
		seen[key] = column.Bucket
	}
	for _, key := range []string{
		"documents.id",
		"workspaces.root_document_id",
		"document_updates.actor_id",
		"activities.provenance_requested_by",
		"agent_events.claimed_by",
	} {
		if _, ok := seen[key]; !ok {
			t.Fatalf("expected %s to be classified", key)
		}
	}
}

func TestUUIDGroup1MigrationStripsPrefixesAndLeavesDocumentIDs(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)
	createLegacyUUIDGroup1Schema(t, db)
	fixture := seedLegacyUUIDGroup1Graph(t, db)
	ids := fixture.ids
	old := fixture.old

	if err := RunUUIDGroup1Migration(t.Context(), db); err != nil {
		t.Fatalf("run migration: %v", err)
	}

	assertColumnType(t, db, "workspaces", "id", "uuid")
	assertColumnType(t, db, "documents", "id", "text")
	assertColumnType(t, db, "workspaces", "root_document_id", "text")
	assertScalar(t, db, `SELECT id::text FROM workspaces`, ids["workspace"])
	assertScalar(t, db, `SELECT workspace_id::text FROM documents`, ids["workspace"])
	assertScalar(t, db, `SELECT id FROM documents`, fixture.documentID)
	assertScalar(t, db, `SELECT root_document_id FROM workspaces`, fixture.rootDocumentID)
	assertScalar(t, db, `SELECT actor_id IS NULL FROM document_updates`, true)
	assertBytes(t, db, `SELECT update FROM document_updates`, fixture.updateBytes)

	for entity, oldID := range map[string]string{
		"workspaces":           old["workspace"],
		"accounts":             old["account"],
		"account_email_tokens": old["token"],
		"workspace_invites":    old["invite"],
		"daemons":              old["daemon"],
		"users":                old["user"],
		"agents":               old["agent"],
		"agent_runs":           old["run"],
		"threads":              old["thread"],
		"thread_messages":      old["message"],
		"agent_events":         old["event"],
	} {
		want := strings.TrimPrefix(oldID, prefixForOldID(oldID))
		assertScalar(t, db, `SELECT new_id::text FROM uuid_migration_map WHERE entity_type = $1 AND old_id = $2`, want, entity, oldID)
	}
	if err := VerifyUUIDGroup1Deep(t.Context(), db); err != nil {
		t.Fatalf("verify migration: %v", err)
	}
	if !isMigratedWorkspaceID(t.Context(), db, old["workspace"]) {
		t.Fatalf("expected old workspace ID %q to be recognized as migrated", old["workspace"])
	}
}

func TestUUIDGroup1AlreadyMigratedStartupUsesBootShapeVerification(t *testing.T) {
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
		t.Fatalf("run migration: %v", err)
	}
	if err := VerifyUUIDGroup1Deep(t.Context(), db); err != nil {
		t.Fatalf("verify migrated fixture: %v", err)
	}

	orphanWorkspaceID := uuid.NewString()
	mustExec(t, db, `UPDATE documents SET workspace_id = $1 WHERE id = $2`, orphanWorkspaceID, fixture.documentID)
	if err := RunUUIDGroup1Migration(t.Context(), db); err != nil {
		t.Fatalf("already-migrated startup should only require boot shape: %v", err)
	}
	if err := VerifyUUIDGroup1BootShape(t.Context(), db); err != nil {
		t.Fatalf("boot-shape verifier: %v", err)
	}
	err = VerifyUUIDGroup1Deep(t.Context(), db)
	if err == nil {
		t.Fatal("expected deep verifier to fail unresolved document workspace reference")
	}
	if !strings.Contains(err.Error(), "documents.workspace_id has") {
		t.Fatalf("deep verifier error = %v, want documents.workspace_id unresolved reference", err)
	}
}

func TestUUIDGroup1MigrationFailsClosedOnInvalidLegacyData(t *testing.T) {
	dsn := postgresTestDSN(t)
	tests := []struct {
		name    string
		seed    func(t *testing.T, db *sql.DB)
		wantErr string
	}{
		{
			name: "missing workspace prefix",
			seed: func(t *testing.T, db *sql.DB) {
				mustExec(t, db, `INSERT INTO workspaces (id, slug, name, root_document_id) VALUES ($1, $2, $3, $4)`,
					"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "missing-prefix", "Missing Prefix", "doc_root_missing")
			},
			wantErr: `missing prefix "ws_"`,
		},
		{
			name: "invalid uuid suffix",
			seed: func(t *testing.T, db *sql.DB) {
				mustExec(t, db, `INSERT INTO workspaces (id, slug, name, root_document_id) VALUES ($1, $2, $3, $4)`,
					"ws_not-a-uuid", "invalid-suffix", "Invalid Suffix", "doc_root_invalid")
			},
			wantErr: "invalid UUID",
		},
		{
			name: "legacy non uuid default",
			seed: func(t *testing.T, db *sql.DB) {
				mustExec(t, db, `INSERT INTO workspaces (id, slug, name, root_document_id) VALUES ($1, $2, $3, $4)`,
					"ws_notty", "legacy-default", "Legacy Default", "doc_root_legacy")
			},
			wantErr: "ws_notty",
		},
		{
			name: "unmapped required reference",
			seed: func(t *testing.T, db *sql.DB) {
				oldWorkspaceID := "ws_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
				mustExec(t, db, `INSERT INTO workspaces (id, slug, name, root_document_id) VALUES ($1, $2, $3, $4)`,
					oldWorkspaceID, "unmapped-ref", "Unmapped Ref", "doc_root_unmapped")
				mustExec(t, db, `INSERT INTO documents (workspace_id, id, path, title) VALUES ($1, $2, $3, $4)`,
					"ws_bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "doc_unmapped", "docs/unmapped.md", "Unmapped")
			},
			wantErr: "documents.workspace_id has 1 unresolved references to workspaces",
		},
		{
			name: "unknown polymorphic actor type",
			seed: func(t *testing.T, db *sql.DB) {
				oldWorkspaceID := "ws_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
				userID := "user_11111111-1111-1111-1111-111111111111"
				mustExec(t, db, `INSERT INTO workspaces (id, slug, name, root_document_id) VALUES ($1, $2, $3, $4)`,
					oldWorkspaceID, "unknown-actor", "Unknown Actor", "doc_root_unknown")
				mustExec(t, db, `INSERT INTO users (workspace_id, id, handle, name) VALUES ($1, $2, $3, $4)`,
					oldWorkspaceID, userID, "ghost", "Ghost")
				mustExec(t, db, `INSERT INTO documents (workspace_id, id, path, title) VALUES ($1, $2, $3, $4)`,
					oldWorkspaceID, "doc_unknown_actor", "docs/actor.md", "Actor")
				mustExec(t, db, `INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type) VALUES ($1, $2, $3, $4, $5)`,
					oldWorkspaceID, "doc_unknown_actor", []byte{0x01}, userID, "robot")
			},
			wantErr: "document_updates.actor_id has unknown actor_type values",
		},
		{
			name: "duplicate stripped uuid",
			seed: func(t *testing.T, db *sql.DB) {
				oldWorkspaceID := "ws_aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
				duplicateUUID := "11111111-1111-1111-1111-111111111111"
				mustExec(t, db, `INSERT INTO workspaces (id, slug, name, root_document_id) VALUES ($1, $2, $3, $4)`,
					oldWorkspaceID, "duplicate-uuid", "Duplicate UUID", "doc_root_duplicate")
				mustExec(t, db, `INSERT INTO users (workspace_id, id, handle, name) VALUES ($1, $2, $3, $4)`,
					oldWorkspaceID, "user_"+duplicateUUID, "dupe-user", "Dupe User")
				mustExec(t, db, `INSERT INTO agents (workspace_id, id, handle, name) VALUES ($1, $2, $3, $4)`,
					oldWorkspaceID, "agent_"+duplicateUUID, "dupe-agent", "Dupe Agent")
			},
			wantErr: "duplicate key value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, err := sql.Open("pgx", dsn)
			if err != nil {
				t.Fatalf("open postgres: %v", err)
			}
			defer db.Close()
			resetUUIDGroup1MigrationTables(t, db)
			createLegacyUUIDGroup1Schema(t, db)
			tt.seed(t, db)

			err = RunUUIDGroup1Migration(t.Context(), db)
			if err == nil {
				t.Fatal("expected migration to fail")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("migration error = %v, want substring %q", err, tt.wantErr)
			}
			assertColumnType(t, db, "workspaces", "id", "text")
			assertScalar(t, db, `
				SELECT EXISTS (
					SELECT 1
					  FROM information_schema.tables
					 WHERE table_schema = 'public'
					   AND table_name = 'uuid_migration_map'
				)`, false)
		})
	}
}

func TestUUIDGroup1MigrationIsIdempotent(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)
	createLegacyUUIDGroup1Schema(t, db)
	seedLegacyUUIDGroup1Graph(t, db)

	if err := RunUUIDGroup1Migration(t.Context(), db); err != nil {
		t.Fatalf("first migration: %v", err)
	}
	if err := RunUUIDGroup1Migration(t.Context(), db); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if err := VerifyUUIDGroup1Deep(t.Context(), db); err != nil {
		t.Fatalf("verify migration: %v", err)
	}
	assertScalar(t, db, `SELECT COUNT(*) FROM uuid_migration_map`, 11)
}

func TestUUIDGroup1MigrationConcurrentRunsSerialize(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)
	createLegacyUUIDGroup1Schema(t, db)
	seedLegacyUUIDGroup1Graph(t, db)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- RunUUIDGroup1Migration(ctx, db)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent migration: %v", err)
		}
	}
	if err := VerifyUUIDGroup1Deep(t.Context(), db); err != nil {
		t.Fatalf("verify migration: %v", err)
	}
	assertScalar(t, db, `SELECT COUNT(*) FROM uuid_migration_map`, 11)
}

func TestUUIDGroup1MigrationOldWorkspaceRoutesReturnGone(t *testing.T) {
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
		t.Fatalf("run migration: %v", err)
	}

	database := &Database{DB: db, URL: dsn}
	if _, err := NewWorkspaceStore(database, fixture.ids["workspace"], "Migrated Workspace"); err != nil {
		t.Fatalf("new store: %v", err)
	}
	server := NewServer(Config{DatabaseURL: dsn, JWTSecret: "test-secret"}, database)
	account := &Account{ID: fixture.ids["account"], Email: "owner@example.test", DisplayName: "Owner", EmailVerified: true}
	humanToken, err := issueJWT("test-secret", account, time.Hour)
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}

	for _, tt := range []struct {
		name  string
		token string
	}{
		{name: "human jwt", token: humanToken},
		{name: "daemon token", token: "stale-daemon-token"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/api/workspaces/"+fixture.old["workspace"]+"/workspace", nil)
			request.Header.Set("Authorization", "Bearer "+tt.token)
			server.Routes().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusGone {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusGone, recorder.Body.String())
			}
		})
	}
}

func TestUUIDGroup1MigrationClaimRouteDoesNotPoisonVerifier(t *testing.T) {
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
		t.Fatalf("run migration: %v", err)
	}

	database2 := &Database{DB: db, URL: dsn}
	store, err := NewWorkspaceStore(database2, fixture.ids["workspace"], "Migrated Workspace")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	router := NewServer(Config{DatabaseURL: dsn, JWTSecret: "test-secret"}, database2).Routes()
	account := &Account{ID: fixture.ids["account"], Email: "owner@example.test", DisplayName: "Owner", EmailVerified: true}
	humanToken, err := issueJWT("test-secret", account, time.Hour)
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}

	tests := []struct {
		name      string
		token     string
		headers   map[string]string
		wantClaim string
	}{
		{name: "human defaults to target agent", token: humanToken, wantClaim: fixture.ids["agent"]},
		{name: "daemon claims as daemon", token: "daemon-token", wantClaim: fixture.ids["daemon"]},
		{name: "acting agent claims as agent", token: "daemon-token", headers: map[string]string{"X-Notty-Acting-Agent-ID": fixture.ids["agent"]}, wantClaim: fixture.ids["agent"]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eventID := seedMigratedPendingAgentEvent(t, db, fixture, tt.name)
			recorder := authTestRequest(t, router, http.MethodPost, "/api/workspaces/"+fixture.ids["workspace"]+"/agent-events/claim", tt.token, tt.headers, ClaimAgentEventRequest{
				AgentID: fixture.ids["agent"],
			})
			if recorder.Code != http.StatusOK {
				t.Fatalf("claim status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
			}
			assertScalar(t, db, `SELECT claimed_by::text FROM agent_events WHERE id = $1`, tt.wantClaim, eventID)
			if err := VerifyUUIDGroup1Deep(t.Context(), db); err != nil {
				t.Fatalf("verify after claim: %v", err)
			}
		})
	}

	recorder := authTestRequest(t, router, http.MethodPost, "/api/workspaces/"+fixture.ids["workspace"]+"/agent-events/claim", humanToken, nil, ClaimAgentEventRequest{
		AgentID:   fixture.ids["agent"],
		ClaimedBy: fixture.ids["user"],
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("human claimed_by=user status = %d, want %d; body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if err := VerifyUUIDGroup1Deep(t.Context(), db); err != nil {
		t.Fatalf("verify after rejected human claim: %v", err)
	}

	otherAgentID := seedMigratedAgent(t, db, fixture, "other-agent")
	if err := store.Reload(); err != nil {
		t.Fatalf("reload store after seeding other agent: %v", err)
	}
	seedMigratedPendingAgentEvent(t, db, fixture, "claim-as-other-agent")
	recorder = authTestRequest(t, router, http.MethodPost, "/api/workspaces/"+fixture.ids["workspace"]+"/agent-events/claim", humanToken, nil, ClaimAgentEventRequest{
		AgentID:   fixture.ids["agent"],
		ClaimedBy: otherAgentID,
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("claimed_by=other-agent status = %d, want %d; body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if _, err := store.DeleteAgent(otherAgentID, OperationMeta{ActorID: fixture.ids["user"], ActorType: "human", Source: "test"}); err != nil {
		t.Fatalf("delete other agent: %v", err)
	}
	if err := VerifyUUIDGroup1Deep(t.Context(), db); err != nil {
		t.Fatalf("verify after rejected cross-agent claim and delete: %v", err)
	}
}

func TestUUIDGroup1MigrationPreservesPostgresAPIsAndCreatesBareUUIDs(t *testing.T) {
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
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("startup migration: %v", err)
	}
	contentDocumentID := queryString(t, db, `SELECT new_id::text FROM uuid_migration_map WHERE entity_type = 'documents' AND old_id = $1`, fixture.documentID)
	rootDocumentID := queryString(t, db, `SELECT new_id::text FROM uuid_migration_map WHERE entity_type = 'documents' AND old_id = $1`, fixture.rootDocumentID)

	workspace, err := getWorkspace(db, fixture.ids["workspace"])
	if err != nil {
		t.Fatalf("get migrated workspace: %v", err)
	}
	if workspace.RootDocumentID != rootDocumentID {
		t.Fatalf("root document ID = %q, want %q", workspace.RootDocumentID, rootDocumentID)
	}
	member, err := workspaceMemberForAccount(db, fixture.ids["workspace"], fixture.ids["account"])
	if err != nil {
		t.Fatalf("get migrated member: %v", err)
	}
	if member.UserID != fixture.ids["user"] {
		t.Fatalf("member user ID = %q, want %q", member.UserID, fixture.ids["user"])
	}

	db.Close()
	db2, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("reopen postgres: %v", err)
	}
	defer db2.Close()
	database3 := &Database{DB: db2, URL: dsn}
	store, err := NewWorkspaceStore(database3, fixture.ids["workspace"], "Migrated Workspace")
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	snapshot := store.Snapshot()
	if snapshot.ContentDocuments[contentDocumentID] == nil {
		t.Fatalf("snapshot missing migrated document %q", contentDocumentID)
	}
	if snapshot.Users[fixture.ids["user"]] == nil {
		t.Fatalf("snapshot missing migrated user %q", fixture.ids["user"])
	}
	if snapshot.Threads[fixture.ids["thread"]] == nil {
		t.Fatalf("snapshot missing migrated thread %q", fixture.ids["thread"])
	}
	if snapshot.AgentEvents[fixture.ids["event"]] == nil {
		t.Fatalf("snapshot missing migrated agent event %q", fixture.ids["event"])
	}

	thread, message, created, err := store.CreateThread(CreateThreadRequest{
		DocumentID: contentDocumentID,
		Body:       "migration smoke thread",
	}, OperationMeta{ActorID: fixture.ids["user"], ActorType: "human", Source: "test"})
	if err != nil {
		t.Fatalf("create migrated thread: %v", err)
	}
	if !created {
		t.Fatal("expected thread to be created")
	}
	if !isUUIDString(thread.ID) || strings.HasPrefix(thread.ID, "thread_") {
		t.Fatalf("thread ID = %q, want bare UUID", thread.ID)
	}
	if message == nil || !isUUIDString(message.ID) || strings.HasPrefix(message.ID, "threadmsg_") {
		t.Fatalf("message ID = %#v, want bare UUID", message)
	}

	account := &Account{ID: fixture.ids["account"], Email: "owner@example.test", DisplayName: "Owner"}
	freshWorkspace, freshMember, err := createWorkspaceForAccount(db2, account, CreateWorkspaceRequest{
		Name:   "Fresh UUID Workspace",
		Slug:   "fresh-uuid-workspace",
		Handle: "freshowner",
	})
	if err != nil {
		t.Fatalf("create fresh workspace: %v", err)
	}
	if !isUUIDString(freshWorkspace.ID) || strings.HasPrefix(freshWorkspace.ID, "ws_") {
		t.Fatalf("fresh workspace ID = %q, want bare UUID", freshWorkspace.ID)
	}
	if !isUUIDString(freshWorkspace.RootDocumentID) || strings.HasPrefix(freshWorkspace.RootDocumentID, "doc_") {
		t.Fatalf("fresh root document ID = %q, want bare UUID", freshWorkspace.RootDocumentID)
	}
	if !isUUIDString(freshMember.UserID) || strings.HasPrefix(freshMember.UserID, "user_") {
		t.Fatalf("fresh member user ID = %q, want bare UUID", freshMember.UserID)
	}
}

func TestUUIDGroup1MigrationPreservesCrossEntityRefs(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)
	createLegacyUUIDGroup1Schema(t, db)
	fixture := seedLegacyUUIDGroup1Graph(t, db)
	old := fixture.old

	// Seed additional cross-entity rows:
	// 1. A second document_update by the agent (polymorphic: actor_type=agent).
	mustExec(t, db, `INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type) VALUES ($1, $2, $3, $4, $5)`,
		old["workspace"], fixture.documentID, []byte{0xaa}, old["agent"], "agent")
	// 2. A third document_update by the user (polymorphic: actor_type=human).
	mustExec(t, db, `INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type) VALUES ($1, $2, $3, $4, $5)`,
		old["workspace"], fixture.documentID, []byte{0xbb}, old["user"], "human")
	// 3. Agent-claimed event (union: claimed_by → daemons|agents).
	mustExec(t, db, `UPDATE agent_events SET claimed_by = $1 WHERE id = $2`, old["agent"], old["event"])

	if err := RunUUIDGroup1Migration(t.Context(), db); err != nil {
		t.Fatalf("run migration: %v", err)
	}

	// Union: both user and agent thread participants must survive.
	assertScalar(t, db, `SELECT COUNT(*) FROM thread_participants WHERE thread_id = $1::uuid`, int64(2), fixture.ids["thread"])
	// Polymorphic: agent and human document_updates actors must survive with correct refs.
	assertScalar(t, db, `SELECT actor_id::text FROM document_updates WHERE actor_type = 'agent'`, fixture.ids["agent"])
	assertScalar(t, db, `SELECT actor_id::text FROM document_updates WHERE actor_type = 'human'`, fixture.ids["user"])
	// Union: agent_events.claimed_by must point to the agent.
	assertScalar(t, db, `SELECT claimed_by::text FROM agent_events WHERE id = $1::uuid`, fixture.ids["agent"], fixture.ids["event"])

	if err := VerifyUUIDGroup1Deep(t.Context(), db); err != nil {
		t.Fatalf("deep verify after migration: %v", err)
	}
}

func TestUUIDGroup1MigrationClearsStagingResidue(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)
	createLegacyUUIDGroup1Schema(t, db)
	fixture := seedLegacyUUIDGroup1Graph(t, db)
	old := fixture.old

	// Staging residue 1: orphaned agent_document_views row (agent deleted from agents table).
	orphanAgentID := "agent_99999999-9999-9999-9999-999999999999"
	mustExec(t, db, `INSERT INTO agent_document_views (workspace_id, agent_id, document_id, update_id, state_vector) VALUES ($1, $2, $3, $4, $5)`,
		old["workspace"], orphanAgentID, fixture.documentID, int64(1), "orphan-sv")

	// Staging residue 2: dead columns that exist in staging but not in code.
	mustExec(t, db, `ALTER TABLE daemons ADD COLUMN IF NOT EXISTS pending_token_hash TEXT NOT NULL DEFAULT ''`)
	mustExec(t, db, `ALTER TABLE workspaces ADD COLUMN IF NOT EXISTS root_stream_id TEXT NOT NULL DEFAULT ''`)

	// Run the full production startup path: initPostgresSchema drops dead
	// columns, sweeps legacy rows, then runs the UUID migration.
	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	// Dead columns dropped by initPostgresSchemaTables.
	for _, col := range []struct{ table, column string }{
		{"daemons", "pending_token_hash"},
		{"workspaces", "root_stream_id"},
	} {
		var exists bool
		db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2)`,
			col.table, col.column).Scan(&exists)
		if exists {
			t.Fatalf("dead column %s.%s should have been dropped by startup", col.table, col.column)
		}
	}

	// Orphaned view row deleted, valid view row survives.
	// Reconnect to clear pgx statement cache after ALTER TABLE.
	db2, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	assertScalar(t, db2, `SELECT COUNT(*) FROM agent_document_views WHERE agent_id = $1::uuid`, int64(1), fixture.ids["agent"])
	assertScalar(t, db2, `SELECT COUNT(*) FROM agent_document_views`, int64(1))

	if err := VerifyUUIDGroup1Deep(t.Context(), db2); err != nil {
		t.Fatalf("deep verify: %v", err)
	}

	// Staging residue 3: data-bearing orphan fails closed.
	// A thread_messages.author_id pointing to a deleted user should be adopted
	// (prefix stripped, added to map) but fail at deep verify because the target
	// entity row doesn't exist.
	db3, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db3.Close()
	resetUUIDGroup1MigrationTables(t, db3)
	createLegacyUUIDGroup1Schema(t, db3)
	fixture = seedLegacyUUIDGroup1Graph(t, db3)
	missingUser := "user_99999999-9999-9999-9999-999999999999"
	mustExec(t, db3, `UPDATE thread_messages SET author_id = $1, author_type = 'human' WHERE id = $2`, missingUser, fixture.old["message"])
	err = RunUUIDGroup1Migration(t.Context(), db3)
	if err == nil {
		t.Fatal("expected data-bearing orphan to fail closed")
	}
	if !strings.Contains(err.Error(), "thread_messages.author_id has") {
		t.Fatalf("migration error = %v, want thread_messages.author_id unresolved reference", err)
	}
}

func TestInitPostgresSchemaMigratesLegacyDocumentHeadsIntoCheckpoints(t *testing.T) {
	dsn := postgresTestDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()
	resetUUIDGroup1MigrationTables(t, db)

	workspaceUUID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	oldWorkspaceID := "ws_" + workspaceUUID
	documentID := "doc_77777777-7777-7777-7777-777777777777"
	mustExec(t, db, `CREATE TABLE workspaces (id TEXT PRIMARY KEY, root_document_id TEXT NOT NULL DEFAULT '')`)
	mustExec(t, db, `CREATE TABLE documents (workspace_id TEXT NOT NULL, id TEXT PRIMARY KEY, path TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', hidden BOOLEAN NOT NULL DEFAULT false, client_id_seed BIGINT NOT NULL DEFAULT 0, create_client_operation_id TEXT NOT NULL DEFAULT '', updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`)
	mustExec(t, db, `CREATE TABLE document_heads (
		workspace_id TEXT NOT NULL,
		document_id TEXT PRIMARY KEY,
		state_vector TEXT NOT NULL DEFAULT '',
		update_id BIGINT NOT NULL DEFAULT 0,
		crdt_state TEXT NOT NULL DEFAULT '',
		crdt_state_update_id BIGINT NOT NULL DEFAULT 0,
		updated_at TIMESTAMPTZ NOT NULL
	)`)
	mustExec(t, db, `INSERT INTO workspaces (id) VALUES ($1)`, oldWorkspaceID)
	mustExec(t, db, `INSERT INTO documents (workspace_id, id, path, title, client_id_seed) VALUES ($1, $2, $3, $4, $5)`, oldWorkspaceID, documentID, "docs/spec.md", "Spec", int64(7))
	mustExec(t, db, `INSERT INTO document_heads (workspace_id, document_id, state_vector, update_id, crdt_state, crdt_state_update_id, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())`, oldWorkspaceID, documentID, "sv", int64(0), "checkpoint-state", int64(9))

	if err := initPostgresSchema(db); err != nil {
		t.Fatalf("init postgres schema: %v", err)
	}

	assertColumnType(t, db, "document_checkpoints", "workspace_id", "uuid")
	assertScalar(t, db, `SELECT workspace_id::text FROM document_heads`, workspaceUUID)
	assertScalar(t, db, `SELECT workspace_id::text FROM document_checkpoints`, workspaceUUID)
	assertScalar(t, db, `SELECT document_id::text FROM document_checkpoints WHERE crdt_state = 'checkpoint-state'`, strings.TrimPrefix(documentID, "doc_"))
	assertScalar(t, db, `SELECT crdt_state FROM document_checkpoints WHERE crdt_state = 'checkpoint-state'`, "checkpoint-state")
	assertScalar(t, db, `SELECT root_document_id IS NOT NULL FROM workspaces`, true)
}

type legacyUUIDGroup1Fixture struct {
	ids            map[string]string
	old            map[string]string
	documentID     string
	rootDocumentID string
	updateBytes    []byte
}

func seedLegacyUUIDGroup1Graph(t *testing.T, db *sql.DB) legacyUUIDGroup1Fixture {
	t.Helper()
	ids := map[string]string{
		"workspace": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"account":   "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		"token":     "cccccccc-cccc-cccc-cccc-cccccccccccc",
		"invite":    "dddddddd-dddd-dddd-dddd-dddddddddddd",
		"daemon":    "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee",
		"user":      "11111111-1111-1111-1111-111111111111",
		"agent":     "22222222-2222-2222-2222-222222222222",
		"run":       "33333333-3333-3333-3333-333333333333",
		"thread":    "44444444-4444-4444-4444-444444444444",
		"message":   "55555555-5555-5555-5555-555555555555",
		"event":     "66666666-6666-6666-6666-666666666666",
	}
	old := map[string]string{
		"workspace": "ws_" + ids["workspace"],
		"account":   "account_" + ids["account"],
		"token":     "account_email_token_" + ids["token"],
		"invite":    "invite_" + ids["invite"],
		"daemon":    "daemon_" + ids["daemon"],
		"user":      "user_" + ids["user"],
		"agent":     "agent_" + ids["agent"],
		"run":       "run_" + ids["run"],
		"thread":    "thread_" + ids["thread"],
		"message":   "threadmsg_" + ids["message"],
		"event":     "aevt_" + ids["event"],
	}
	documentID := "doc_77777777-7777-7777-7777-777777777777"
	rootDocumentID := "doc_root_" + old["workspace"]
	updateBytes := []byte{0x01, 0x02, 0x03, 0xff}

	mustExec(t, db, `INSERT INTO workspaces (id, slug, name, root_document_id) VALUES ($1, $2, $3, $4)`, old["workspace"], "legacy-workspace", "Legacy Workspace", rootDocumentID)
	mustExec(t, db, `INSERT INTO accounts (id, email, display_name, last_accessed_workspace_id) VALUES ($1, $2, $3, $4)`, old["account"], "owner@example.test", "Owner", old["workspace"])
	mustExec(t, db, `INSERT INTO account_email_tokens (id, account_id, purpose, token_hash, expires_at) VALUES ($1, $2, $3, $4, NOW() + INTERVAL '1 hour')`, old["token"], old["account"], "verify", "token-hash")
	mustExec(t, db, `INSERT INTO workspace_invites (id, workspace_id, created_by_user_id, token_hash, expires_at) VALUES ($1, $2, $3, $4, NOW() + INTERVAL '1 hour')`, old["invite"], old["workspace"], old["user"], "invite-hash")
	mustExec(t, db, `INSERT INTO daemons (id, workspace_id, name, token_hash, status) VALUES ($1, $2, $3, $4, $5)`, old["daemon"], old["workspace"], "Daemon", tokenHash("daemon-token"), "active")
	mustExec(t, db, `INSERT INTO documents (workspace_id, id, path, title, client_id_seed) VALUES ($1, $2, $3, $4, $5)`, old["workspace"], documentID, "docs/spec.md", "Spec", int64(42))
	mustExec(t, db, `INSERT INTO document_heads (workspace_id, document_id, state_vector, update_id) VALUES ($1, $2, $3, $4)`, old["workspace"], documentID, "sv", int64(1))
	mustExec(t, db, `INSERT INTO document_updates (workspace_id, document_id, update, actor_id, actor_type) VALUES ($1, $2, $3, $4, $5)`, old["workspace"], documentID, updateBytes, "system", "system")
	mustExec(t, db, `INSERT INTO document_checkpoints (workspace_id, document_id, update_id, crdt_state, state_vector) VALUES ($1, $2, $3, $4, $5)`, old["workspace"], documentID, int64(1), "checkpoint", "csv")
	mustExec(t, db, `INSERT INTO users (workspace_id, id, handle, name, role, kind, status) VALUES ($1, $2, $3, $4, $5, $6, $7)`, old["workspace"], old["user"], "owner", "Owner", "Workspace owner", "human", "active")
	mustExec(t, db, `INSERT INTO agents (workspace_id, id, daemon_id, current_run_id, handle, name, role, kind, status) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, old["workspace"], old["agent"], old["daemon"], old["run"], "agent", "Agent", "Assistant", "codex", "idle")
	mustExec(t, db, `INSERT INTO agent_runs (workspace_id, id, agent_id, agent_handle, agent_name, agent_kind, status, desired_status) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, old["workspace"], old["run"], old["agent"], "agent", "Agent", "codex", "completed", "completed")
	mustExec(t, db, `INSERT INTO threads (workspace_id, id, document_id, created_by_id, created_by_type, title, status, created_by_handle, created_by_name) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, old["workspace"], old["thread"], documentID, old["user"], "human", "Thread", "open", "owner", "Owner")
	mustExec(t, db, `INSERT INTO thread_messages (workspace_id, id, thread_id, author_id, author_type, author_handle, author_name, body, kind) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, old["workspace"], old["message"], old["thread"], old["user"], "human", "owner", "Owner", "hello", "comment")
	mustExec(t, db, `INSERT INTO thread_participants (workspace_id, thread_id, participant_id) VALUES ($1, $2, $3), ($1, $2, $4)`, old["workspace"], old["thread"], old["user"], old["agent"])
	mustExec(t, db, `INSERT INTO presences (workspace_id, actor_id, actor_type, document_id, file_path, mode, activity) VALUES ($1, $2, $3, $4, $5, $6, $7)`, old["workspace"], old["daemon"], "daemon", documentID, "docs/spec.md", "editing", "active")
	mustExec(t, db, `INSERT INTO activities (workspace_id, type, document_id, actor_id, actor_type, summary, provenance_actor_id, provenance_actor_type, provenance_requested_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, old["workspace"], "document.updated", documentID, old["user"], "human", "updated", old["agent"], "agent", "opaque-requester")
	mustExec(t, db, `INSERT INTO agent_events (workspace_id, id, agent_id, agent_handle, type, status, document_id, thread_id, thread_message_id, claimed_by, run_id) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`, old["workspace"], old["event"], old["agent"], "agent", "message", "completed", documentID, old["thread"], old["message"], old["daemon"], old["run"])
	mustExec(t, db, `INSERT INTO agent_document_views (workspace_id, agent_id, document_id, update_id, state_vector) VALUES ($1, $2, $3, $4, $5)`, old["workspace"], old["agent"], documentID, int64(1), "view-sv")
	mustExec(t, db, `INSERT INTO workspace_members (workspace_id, account_id, user_id, membership_role, status, invited_by, last_accessed_document_id) VALUES ($1, $2, $3, $4, $5, $3, $6)`, old["workspace"], old["account"], old["user"], MembershipRoleOwner, "active", documentID)

	return legacyUUIDGroup1Fixture{
		ids:            ids,
		old:            old,
		documentID:     documentID,
		rootDocumentID: rootDocumentID,
		updateBytes:    updateBytes,
	}
}

func seedMigratedPendingAgentEvent(t *testing.T, db *sql.DB, fixture legacyUUIDGroup1Fixture, dedup string) string {
	t.Helper()
	eventID := uuid.NewString()
	mustExec(t, db, `
		INSERT INTO agent_events (
			workspace_id, id, agent_id, agent_handle, type, box, status, document_id,
			summary, prompt, dedup_key, last_error, available_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NOW(), NOW(), NOW())`,
		fixture.ids["workspace"], eventID, fixture.ids["agent"], "agent", "message", "for_me", "pending",
		fixture.documentID, "claim smoke", "claim smoke", "claim-"+strings.ReplaceAll(dedup, " ", "-"), "",
	)
	return eventID
}

func seedMigratedAgent(t *testing.T, db *sql.DB, fixture legacyUUIDGroup1Fixture, handle string) string {
	t.Helper()
	agentID := uuid.NewString()
	mustExec(t, db, `
		INSERT INTO agents (
			workspace_id, id, daemon_id, handle, name, role, kind, system_prompt,
			workspace_root, status, current_task, current_activity, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NOW())`,
		fixture.ids["workspace"], agentID, fixture.ids["daemon"], handle, "Other Agent", "Assistant",
		"codex", "", "agents/"+agentID, "idle", "", "",
	)
	return agentID
}

func createLegacyUUIDGroup1Schema(t *testing.T, db *sql.DB) {
	t.Helper()
	statements := []string{
		`CREATE TABLE workspaces (id TEXT PRIMARY KEY, slug TEXT UNIQUE NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '', root_document_id TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE accounts (id TEXT PRIMARY KEY, email TEXT UNIQUE NOT NULL DEFAULT '', display_name TEXT NOT NULL DEFAULT '', password_hash TEXT NOT NULL DEFAULT '', email_verified BOOLEAN NOT NULL DEFAULT TRUE, last_accessed_workspace_id TEXT, password_updated_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE account_email_tokens (id TEXT PRIMARY KEY, account_id TEXT NOT NULL, purpose TEXT NOT NULL DEFAULT '', token_hash TEXT NOT NULL DEFAULT '', expires_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), consumed_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE workspace_members (workspace_id TEXT NOT NULL, account_id TEXT NOT NULL, user_id TEXT NOT NULL, membership_role TEXT NOT NULL DEFAULT 'member', status TEXT NOT NULL DEFAULT 'active', invited_by TEXT, last_accessed_document_id TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), accepted_at TIMESTAMPTZ DEFAULT NOW(), PRIMARY KEY (workspace_id, account_id))`,
		`CREATE TABLE workspace_invites (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, created_by_user_id TEXT NOT NULL, token_hash TEXT NOT NULL DEFAULT '', expires_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE daemons (id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL, name TEXT NOT NULL DEFAULT '', token_hash TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'active', daemon_version TEXT NOT NULL DEFAULT '', os TEXT NOT NULL DEFAULT '', arch TEXT NOT NULL DEFAULT '', runtime_detections JSONB NOT NULL DEFAULT '[]'::jsonb, last_seen_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), deleted_at TIMESTAMPTZ)`,
		`CREATE TABLE documents (workspace_id TEXT NOT NULL, id TEXT PRIMARY KEY, path TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', hidden BOOLEAN NOT NULL DEFAULT false, client_id_seed BIGINT NOT NULL DEFAULT 0, create_client_operation_id TEXT NOT NULL DEFAULT '', updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE document_heads (workspace_id TEXT NOT NULL, document_id TEXT PRIMARY KEY, state_vector TEXT NOT NULL DEFAULT '', update_id BIGINT NOT NULL DEFAULT 0, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE document_updates (id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL, document_id TEXT NOT NULL, update BYTEA NOT NULL, actor_id TEXT, actor_type TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE document_checkpoints (id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL, document_id TEXT NOT NULL, update_id BIGINT NOT NULL DEFAULT 0, crdt_state TEXT NOT NULL DEFAULT '', state_vector TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), UNIQUE (workspace_id, document_id, update_id))`,
		`CREATE TABLE users (workspace_id TEXT NOT NULL, id TEXT PRIMARY KEY, handle TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '', role TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE agents (workspace_id TEXT NOT NULL, id TEXT PRIMARY KEY, daemon_id TEXT, current_run_id TEXT, handle TEXT NOT NULL DEFAULT '', name TEXT NOT NULL DEFAULT '', role TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT '', system_prompt TEXT NOT NULL DEFAULT '', workspace_root TEXT NOT NULL DEFAULT '', current_turn_id TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', current_task TEXT NOT NULL DEFAULT '', current_activity TEXT NOT NULL DEFAULT '', last_heartbeat_at TIMESTAMPTZ, last_run_completed TIMESTAMPTZ, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE agent_runs (workspace_id TEXT NOT NULL, id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, agent_handle TEXT NOT NULL DEFAULT '', agent_name TEXT NOT NULL DEFAULT '', agent_kind TEXT NOT NULL DEFAULT '', system_prompt TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '', workspace_root TEXT NOT NULL DEFAULT '', working_dir TEXT NOT NULL DEFAULT '', prompt TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', desired_status TEXT NOT NULL DEFAULT '', process_id INTEGER, launch_time TIMESTAMPTZ, last_heartbeat_at TIMESTAMPTZ, completed_at TIMESTAMPTZ, exit_code INTEGER, last_message TEXT NOT NULL DEFAULT '', log_tail JSONB NOT NULL DEFAULT '[]'::jsonb, error TEXT NOT NULL DEFAULT '', assigned_task_ref TEXT NOT NULL DEFAULT '', updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE threads (workspace_id TEXT NOT NULL, id TEXT PRIMARY KEY, document_id TEXT NOT NULL, created_by_id TEXT NOT NULL, created_by_type TEXT NOT NULL DEFAULT '', client_operation_id TEXT NOT NULL DEFAULT '', title TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT '', anchor_relative_start TEXT NOT NULL DEFAULT '', anchor_relative_end TEXT NOT NULL DEFAULT '', anchor_kind TEXT NOT NULL DEFAULT '', anchor_excerpt TEXT NOT NULL DEFAULT '', created_by_handle TEXT NOT NULL DEFAULT '', created_by_name TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE thread_messages (workspace_id TEXT NOT NULL, id TEXT PRIMARY KEY, thread_id TEXT NOT NULL, author_id TEXT NOT NULL, author_type TEXT NOT NULL DEFAULT '', author_handle TEXT NOT NULL DEFAULT '', author_name TEXT NOT NULL DEFAULT '', body TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE thread_participants (workspace_id TEXT NOT NULL, thread_id TEXT NOT NULL, participant_id TEXT NOT NULL)`,
		`CREATE TABLE presences (workspace_id TEXT NOT NULL, actor_id TEXT NOT NULL, actor_type TEXT NOT NULL DEFAULT '', document_id TEXT NOT NULL DEFAULT '', file_path TEXT NOT NULL DEFAULT '', mode TEXT NOT NULL DEFAULT '', selection_start INTEGER, selection_end INTEGER, activity TEXT NOT NULL DEFAULT '', updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (workspace_id, actor_id))`,
		`CREATE TABLE activities (id BIGSERIAL PRIMARY KEY, workspace_id TEXT NOT NULL, type TEXT NOT NULL DEFAULT '', document_id TEXT NOT NULL DEFAULT '', actor_id TEXT, actor_type TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '', occurred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), provenance_actor_id TEXT, provenance_actor_type TEXT NOT NULL DEFAULT '', provenance_execution_id TEXT NOT NULL DEFAULT '', provenance_tool TEXT NOT NULL DEFAULT '', provenance_trigger TEXT NOT NULL DEFAULT '', provenance_autonomous BOOLEAN NOT NULL DEFAULT false, provenance_confidence TEXT NOT NULL DEFAULT '', provenance_requested_by TEXT NOT NULL DEFAULT '', provenance_source TEXT NOT NULL DEFAULT '', provenance_intended_scope TEXT NOT NULL DEFAULT '', provenance_read_set_summary TEXT NOT NULL DEFAULT '', comment_id TEXT NOT NULL DEFAULT '', presence_ref TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE agent_events (workspace_id TEXT NOT NULL, id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, agent_handle TEXT NOT NULL DEFAULT '', type TEXT NOT NULL DEFAULT '', box TEXT NOT NULL DEFAULT 'for_me', status TEXT NOT NULL DEFAULT '', document_id TEXT NOT NULL DEFAULT '', thread_id TEXT, thread_message_id TEXT, from_update_id BIGINT NOT NULL DEFAULT 0, to_update_id BIGINT NOT NULL DEFAULT 0, summary TEXT NOT NULL DEFAULT '', prompt TEXT NOT NULL DEFAULT '', dedup_key TEXT NOT NULL DEFAULT '', claimed_by TEXT, run_id TEXT, last_error TEXT NOT NULL DEFAULT '', attempt_count INTEGER NOT NULL DEFAULT 0, available_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), claimed_at TIMESTAMPTZ, completed_at TIMESTAMPTZ, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW())`,
		`CREATE TABLE agent_document_views (workspace_id TEXT NOT NULL, agent_id TEXT NOT NULL, document_id TEXT NOT NULL, update_id BIGINT NOT NULL DEFAULT 0, state_vector TEXT NOT NULL DEFAULT '', viewed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (workspace_id, agent_id, document_id))`,
	}
	for _, statement := range statements {
		mustExec(t, db, statement)
	}
}

func resetUUIDGroup1MigrationTables(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{
		"uuid_migration_map",
		"agent_document_views",
		"agent_events",
		"activities",
		"presences",
		"thread_participants",
		"thread_messages",
		"threads",
		"agent_runs",
		"agents",
		"users",
		"document_checkpoints",
		"document_updates",
		"document_heads",
		"documents",
		"daemons",
		"workspace_invites",
		"workspace_members",
		"account_email_tokens",
		"accounts",
		"workspaces",
	} {
		mustExec(t, db, `DROP TABLE IF EXISTS `+table+` CASCADE`)
	}
}

func assertColumnType(t *testing.T, db *sql.DB, table, column, want string) {
	t.Helper()
	var got string
	if err := db.QueryRow(`
		SELECT data_type
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND table_name = $1
		   AND column_name = $2`,
		table, column,
	).Scan(&got); err != nil {
		t.Fatalf("column type %s.%s: %v", table, column, err)
	}
	if got != want {
		t.Fatalf("%s.%s type = %q, want %q", table, column, got, want)
	}
}

func assertScalar[T comparable](t *testing.T, db *sql.DB, query string, want T, args ...any) {
	t.Helper()
	var got T
	if err := db.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatalf("query scalar %q: %v", query, err)
	}
	if got != want {
		t.Fatalf("query %q = %#v, want %#v", query, got, want)
	}
}

func assertBytes(t *testing.T, db *sql.DB, query string, want []byte, args ...any) {
	t.Helper()
	var got []byte
	if err := db.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatalf("query bytes %q: %v", query, err)
	}
	if string(got) != string(want) {
		t.Fatalf("query %q bytes = %v, want %v", query, got, want)
	}
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func prefixForOldID(value string) string {
	for _, prefix := range []string{
		"account_email_token_",
		"threadmsg_",
		"account_",
		"invite_",
		"daemon_",
		"agent_",
		"thread_",
		"aevt_",
		"user_",
		"run_",
		"ws_",
	} {
		if strings.HasPrefix(value, prefix) {
			return prefix
		}
	}
	return ""
}

func TestUUIDStringOrNilRejectsNonUUIDOptionalRefs(t *testing.T) {
	if uuidStringOrNil("") != nil {
		t.Fatal("empty optional UUID should map to nil")
	}
	if uuidStringOrNil("daemon") != nil {
		t.Fatal("legacy claimed_by daemon sentinel should map to nil")
	}
	id := uuid.NewString()
	if got := uuidStringOrNil(id); got != id {
		t.Fatalf("uuidStringOrNil(%q) = %#v", id, got)
	}
}
