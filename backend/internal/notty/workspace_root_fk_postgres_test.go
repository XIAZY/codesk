package notty

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func assertSQLCount(t *testing.T, db *sql.DB, want int, query string, args ...any) {
	t.Helper()
	var got int
	if err := db.QueryRow(query, args...).Scan(&got); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	if got != want {
		t.Fatalf("count query %q = %d, want %d", query, got, want)
	}
}

func workspaceRootDocumentID(t *testing.T, db *sql.DB, workspaceID string) string {
	t.Helper()
	var rootID string
	if err := db.QueryRow(`SELECT root_document_id::text FROM workspaces WHERE id = $1::uuid`, workspaceID).Scan(&rootID); err != nil {
		t.Fatalf("select root_document_id: %v", err)
	}
	if rootID == "" {
		t.Fatal("workspace has no root_document_id")
	}
	return rootID
}

// TestWorkspaceCreationSeedsRootDocumentAtomically verifies that creating a
// workspace through the real HTTP path also creates its root document — the
// documents row, its head, and its initial update — in the same commit, and
// that the root loads and is syncable without any lazy bootstrap.
func TestWorkspaceCreationSeedsRootDocumentAtomically(t *testing.T) {
	server, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "atomic-owner@example.com", "owner-pass", "Atomic Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Atomic Tenant")
	db := server.sqlDB()

	rootID := workspaceRootDocumentID(t, db, workspace.ID)
	assertSQLCount(t, db, 1, `SELECT count(*) FROM documents WHERE workspace_id = $1::uuid AND id = $2::uuid`, workspace.ID, rootID)
	assertSQLCount(t, db, 1, `SELECT count(*) FROM document_heads WHERE workspace_id = $1::uuid AND document_id = $2::uuid`, workspace.ID, rootID)
	assertSQLCount(t, db, 1, `SELECT count(*) FROM document_updates WHERE workspace_id = $1::uuid AND document_id = $2::uuid`, workspace.ID, rootID)

	store, err := server.workspaceStore(workspace.ID)
	if err != nil {
		t.Fatalf("open workspace store: %v", err)
	}
	if got := store.Snapshot().RootDocumentID; got != rootID {
		t.Fatalf("store root document id = %q, want %q", got, rootID)
	}
	if !store.HasDocument(rootID) {
		t.Fatalf("root document %q is not syncable after creation", rootID)
	}
}

// TestWorkspaceRootDocumentFKRejectsDanglingRoot proves the deferred constraint
// refuses a workspace whose root_document_id has no documents row.
func TestWorkspaceRootDocumentFKRejectsDanglingRoot(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB

	wsID := uuid.NewString()
	rootID := uuid.NewString()
	_, err := db.Exec(
		`INSERT INTO workspaces (id, slug, name, root_document_id, created_at, updated_at)
		 VALUES ($1, $2, 'Dangling', $3, now(), now())`,
		wsID, "dangling-"+wsID[:8], rootID,
	)
	if err == nil {
		t.Fatal("expected FK violation inserting a workspace with no root document row")
	}
	if !strings.Contains(err.Error(), "fk_workspaces_root_document") {
		t.Fatalf("expected fk_workspaces_root_document violation, got: %v", err)
	}
}

// TestWorkspaceRootDocumentDeleteRestricted proves ON DELETE RESTRICT refuses to
// delete a workspace's root document, while non-root documents remain deletable.
func TestWorkspaceRootDocumentDeleteRestricted(t *testing.T) {
	server, router := newAuthTestServer(t)
	owner := authTestRegister(t, router, "restrict-owner@example.com", "owner-pass", "Restrict Owner")
	workspace := authTestCreateWorkspace(t, router, owner.Token, "Restrict Tenant")
	db := server.sqlDB()
	rootID := workspaceRootDocumentID(t, db, workspace.ID)

	if _, err := db.Exec(`DELETE FROM documents WHERE workspace_id = $1::uuid AND id = $2::uuid`, workspace.ID, rootID); err == nil {
		t.Fatal("expected RESTRICT to refuse deleting a workspace's root document")
	} else if !strings.Contains(err.Error(), "fk_workspaces_root_document") {
		t.Fatalf("expected fk_workspaces_root_document restriction, got: %v", err)
	}

	// A non-root document is deletable — the RESTRICT is specific to the root.
	otherDoc := uuid.NewString()
	if _, err := db.Exec(`INSERT INTO documents (workspace_id, id, hidden, client_id_seed, updated_at) VALUES ($1, $2, false, 1002, now())`, workspace.ID, otherDoc); err != nil {
		t.Fatalf("insert non-root document: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM documents WHERE workspace_id = $1::uuid AND id = $2::uuid`, workspace.ID, otherDoc); err != nil {
		t.Fatalf("deleting a non-root document should succeed, got: %v", err)
	}
}

// TestStoreLoadFailsClosedWhenRootDocumentMissing forces the unrepresentable
// state the constraint normally prevents and asserts the store fails closed
// (the deleted from-scratch branch is now a tripwire, not a silent re-seed).
func TestStoreLoadFailsClosedWhenRootDocumentMissing(t *testing.T) {
	database := newPostgresTestDatabase(t)
	db := database.DB
	workspace := insertPostgresTestWorkspace(t, database, "Tripwire")

	if _, err := db.Exec(`ALTER TABLE workspaces DROP CONSTRAINT IF EXISTS fk_workspaces_root_document`); err != nil {
		t.Fatalf("drop constraint: %v", err)
	}
	for _, stmt := range []string{
		`DELETE FROM document_heads WHERE workspace_id = $1::uuid AND document_id = $2::uuid`,
		`DELETE FROM document_updates WHERE workspace_id = $1::uuid AND document_id = $2::uuid`,
		`DELETE FROM documents WHERE workspace_id = $1::uuid AND id = $2::uuid`,
	} {
		if _, err := db.Exec(stmt, workspace.ID, workspace.RootDocumentID); err != nil {
			t.Fatalf("force missing root document: %v", err)
		}
	}

	if _, err := NewWorkspaceStore(database, workspace.ID, workspace.Name); err == nil {
		t.Fatal("expected store load to fail closed when the root document is missing")
	} else if !strings.Contains(err.Error(), "missing root document") {
		t.Fatalf("expected 'missing root document' tripwire, got: %v", err)
	}
}
